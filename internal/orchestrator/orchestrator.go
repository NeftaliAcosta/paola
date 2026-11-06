package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/neftaliacosta/paola/internal/agent"
	"github.com/neftaliacosta/paola/internal/config"
	"github.com/neftaliacosta/paola/internal/gitutil"
	"github.com/neftaliacosta/paola/internal/history"
)

type LogFunc func(string)
type ProgressFunc func(Progress)

type Task struct {
	Description string
	Status      string // done, current, pending
}

type Progress struct {
	Project     string
	Tasks       []Task
	CurrentTask int
	TotalTasks  int
	Attempt     int
	MaxRetries  int
	Role        string
	CLI         string
	RoleAgents  map[string]string
	Verdict     string
	Paused      bool
}

type Control struct {
	paused  atomic.Bool
	skipped atomic.Bool
	cancel  context.CancelFunc
}

func NewControl(cancel context.CancelFunc) *Control { return &Control{cancel: cancel} }
func (c *Control) TogglePause() bool                { c.paused.Store(!c.paused.Load()); return c.paused.Load() }
func (c *Control) IsPaused() bool                   { return c != nil && c.paused.Load() }
func (c *Control) Skip() {
	if c != nil {
		c.skipped.Store(true)
	}
}
func (c *Control) ConsumeSkip() bool { return c != nil && c.skipped.Swap(false) }
func (c *Control) Quit() {
	if c != nil && c.cancel != nil {
		c.cancel()
	}
}
func (c *Control) Wait(ctx context.Context) error {
	for c != nil && c.paused.Load() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return ctx.Err()
}

type Engine struct {
	Root       string
	Config     config.Config
	Runner     agent.AgentRunner
	Log        LogFunc
	Update     ProgressFunc
	Control    *Control
	TaskBranch string
	BaseBranch string
	Sessions   map[string]string
}

func (e *Engine) emit(s string) {
	if e.Log != nil {
		e.Log(s)
	}
}

func (e *Engine) Run(ctx context.Context) (runErr error) {
	roadmapPath := filepath.Join(e.Root, config.Dir, "ROADMAP.md")
	b, err := os.ReadFile(roadmapPath)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	state, _ := config.LoadState(e.Root)
	if e.Sessions == nil {
		e.Sessions = state.Sessions
	}
	if e.Sessions == nil {
		e.Sessions = map[string]string{}
	}
	maxRetries := e.maxRetries()
	indexes := pendingTaskIndexes(lines)
	if len(indexes) == 0 {
		// A completed roadmap is not a new execution. Avoid creating an empty
		// history manifest when the user invokes `paola run` again by mistake.
		return nil
	}
	record, err := history.Start(e.Root, e.Config)
	if err != nil {
		return err
	}
	runStatus := "failed"
	promptPath := filepath.Join(e.Root, config.Dir, "state", "current_prompt.md")
	_ = os.Remove(promptPath)
	defer func() { _ = os.Remove(promptPath) }()
	defer func() {
		state.Sessions = e.Sessions
		_ = config.SaveState(e.Root, state)
		if runErr != nil && ctx.Err() != nil {
			runStatus = "interrupted"
			state.Status = "interrupted"
			_ = config.SaveState(e.Root, state)
		}
		_ = record.FinishWithError(runStatus, errorText(runErr))
	}()
	if state.Status == "" {
		state.Status = "running"
	}
	for ordinal, idx := range indexes {
		task := gitutil.CleanTask(lines[idx])
		_ = os.Remove(promptPath)
		_ = record.RecordTask(history.TaskRecord{Index: ordinal + 1, Description: task, Status: "running", StartedAt: time.Now().UTC()})
		state.CurrentTask = ordinal
		state.Attempt = 0
		state.Status = "running"
		if err := config.SaveState(e.Root, state); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(e.Root, config.Dir, "state", "feedback.md"), []byte("Initial attempt — no previous feedback.\n"), 0644); err != nil {
			return err
		}
		e.progress(taskList(lines, idx), -1, ordinal+1, 0, "", "", "")
		e.emit("▶ " + task)

		completed := false
		reviewRounds := 0
		metrics := map[string]history.AgentMetric{}
		turns := map[string]int{}
		for !completed {
			if err := e.wait(ctx); err != nil {
				return err
			}
			if e.Control != nil && e.Control.ConsumeSkip() {
				e.emit("↷ skipped: " + task)
				break
			}
			state.Attempt++
			if err := config.SaveState(e.Root, state); err != nil {
				return err
			}
			coder := e.Config.Agents["coder"]
			e.progress(taskList(lines, idx), -1, ordinal+1, state.Attempt, "Coder", coder, "")
			prompt, _ := e.prompt("coder", task)
			_ = os.WriteFile(filepath.Join(e.Root, config.Dir, "state", "current_prompt.md"), []byte(prompt), 0600)
			e.emit(fmt.Sprintf("Coder attempt %d/%d (%s)", state.Attempt, maxRetries, coder))
			coderOutput, err := e.runRole(ctx, "coder", prompt, turns, metrics)
			_ = record.Log(ordinal+1, "coder", coderOutput)
			if err != nil {
				_ = record.UpdateTaskMetrics(ordinal+1, metrics)
				if retryableError(err) && state.Attempt < maxRetries {
					e.emit(fmt.Sprintf("Retryable coder failure; retrying in %ds", e.Config.EffectiveRetryBackoffSeconds()))
					if err := waitBackoff(ctx, e.Config.EffectiveRetryBackoffSeconds()); err != nil {
						return err
					}
					continue
				}
				return err
			}

			if e.Config.TestCommand != "" {
				testOut, testErr := runTest(ctx, e.Root, e.Config.TestCommand)
				if err := os.WriteFile(filepath.Join(e.Root, config.Dir, "state", "test_output.log"), []byte(testOut), 0644); err != nil {
					return err
				}
				if testErr != nil {
					if err := os.WriteFile(filepath.Join(e.Root, config.Dir, "state", "feedback.md"), []byte(testOut), 0644); err != nil {
						return err
					}
					e.emit("Tests failed; returning to Coder")
					_ = writeContextSummary(e.Root, task, metrics)
					if state.Attempt >= maxRetries {
						return fmt.Errorf("tests failed after %d attempts", state.Attempt)
					}
					continue
				}
			}

			diff := ""
			if e.Config.Simulation {
				diff = "SIMULATION: no project diff captured.\n"
			} else {
				var diffErr error
				diff, diffErr = gitutil.CaptureDiff(e.Root)
				if diffErr != nil {
					return fmt.Errorf("capture Git diff for task %q: %w", task, diffErr)
				}
			}
			if strings.TrimSpace(diff) == "" && !e.Config.Simulation {
				return fmt.Errorf("coder completed without project changes for task %q; task gate blocked advancement", task)
			}
			if err := os.WriteFile(gitutil.SafeDiffPath(e.Root), []byte(diff), 0644); err != nil {
				return err
			}
			_ = record.Diff(ordinal+1, diff)
			reviewer := e.Config.Agents["reviewer"]
			reviewRounds++
			e.progress(taskList(lines, idx), -1, ordinal+1, state.Attempt, "Reviewer", reviewer, "")
			reviewPrompt, _ := e.prompt("reviewer", task)
			e.emit("Reviewer (" + reviewer + ")")
			reviewInput := e.limitRoleInput("reviewer", reviewPrompt+"\nCURRENT DIFF:\n"+diff)
			review, err := e.runRole(ctx, "reviewer", reviewInput, turns, metrics)
			_ = record.Log(ordinal+1, "reviewer", review)
			if err != nil {
				_ = record.UpdateTaskMetrics(ordinal+1, metrics)
				if retryableError(err) && state.Attempt < maxRetries {
					e.emit(fmt.Sprintf("Retryable reviewer failure; retrying in %ds", e.Config.EffectiveRetryBackoffSeconds()))
					if err := waitBackoff(ctx, e.Config.EffectiveRetryBackoffSeconds()); err != nil {
						return err
					}
					continue
				}
				return err
			}
			if err := os.WriteFile(filepath.Join(e.Root, config.Dir, "state", "feedback.md"), []byte(review), 0644); err != nil {
				return err
			}
			if hasMarker(review, "STATUS: LGTM") {
				completed = true
				e.emit("✓ reviewer approved")
				break
			}
			if state.Attempt < maxRetries {
				e.emit("Reviewer requested changes")
				_ = writeContextSummary(e.Root, task, metrics)
				continue
			}

			judge := e.Config.Agents["judge"]
			e.progress(taskList(lines, idx), -1, ordinal+1, state.Attempt, "Judge", judge, "")
			judgePrompt, _ := e.prompt("judge", task)
			e.emit("Judge (" + judge + ")")
			judgeInput := e.limitRoleInput("judge", judgePrompt+"\nREVIEW:\n"+review+"\nCURRENT DIFF:\n"+diff)
			verdict, err := e.runRole(ctx, "judge", judgeInput, turns, metrics)
			_ = record.Log(ordinal+1, "judge", verdict)
			if err != nil {
				_ = record.UpdateTaskMetrics(ordinal+1, metrics)
				return err
			}
			if err := os.WriteFile(filepath.Join(e.Root, config.Dir, "state", "feedback.md"), []byte(verdict), 0644); err != nil {
				return err
			}
			if hasMarker(verdict, "VERDICT: OVERRIDE_APPROVE") {
				completed = true
				e.emit("✓ judge approved")
				break
			}
			if !hasMarker(verdict, "VERDICT: FINAL_INSTRUCTIONS") {
				return fmt.Errorf("judge returned no recognized verdict for task %q", task)
			}
			// The Judge's synthesis becomes feedback for exactly one final Coder turn.
			e.emit("Judge issued final instructions; starting bonus Coder turn")
			if err := e.wait(ctx); err != nil {
				return err
			}
			state.Attempt++
			coderPrompt, _ := e.prompt("coder", task)
			e.progress(taskList(lines, idx), -1, ordinal+1, state.Attempt, "Coder (bonus)", coder, "FINAL_INSTRUCTIONS")
			bonusOutput, err := e.runRole(ctx, "coder", coderPrompt, turns, metrics)
			_ = record.Log(ordinal+1, "coder-bonus", bonusOutput)
			if err != nil {
				_ = record.UpdateTaskMetrics(ordinal+1, metrics)
				return fmt.Errorf("final coder turn failed: %w", err)
			}
			if e.Config.TestCommand != "" {
				testOut, testErr := runTest(ctx, e.Root, e.Config.TestCommand)
				if err := os.WriteFile(filepath.Join(e.Root, config.Dir, "state", "test_output.log"), []byte(testOut), 0644); err != nil {
					return err
				}
				if testErr != nil {
					return fmt.Errorf("final coder tests failed: %w", testErr)
				}
			}
			finalDiff, err := gitutil.CaptureDiff(e.Root)
			if err != nil {
				return err
			}
			if err := os.WriteFile(gitutil.SafeDiffPath(e.Root), []byte(finalDiff), 0644); err != nil {
				return err
			}
			_ = record.Diff(ordinal+1, finalDiff)
			completed = true
			e.emit("✓ final coder turn complete")
		}
		if completed {
			_ = record.UpdateTask(ordinal+1, "completed", "", taskTokenCount(e.Root, metrics), gitutil.ChangedFiles(e.Root), state.Attempt, reviewRounds)
			_ = record.UpdateTaskMetrics(ordinal+1, metrics)
			_ = writeContextSummary(e.Root, task, metrics)
			_ = os.Remove(promptPath)
			if !e.Config.Simulation {
				if err := completeTask(e.Root, roadmapPath, lines, idx, task, state); err != nil {
					return err
				}
			}
			lines[idx] = strings.Replace(lines[idx], "- [ ]", "- [x]", 1)
		}
	}
	state.Status = "complete"
	runStatus = "completed"
	state.Attempt = 0
	if e.TaskBranch != "" {
		e.emit(fmt.Sprintf("All tasks completed on branch '%s'. Review your changes with 'git log' or merge into '%s'.", e.TaskBranch, e.BaseBranch))
	}
	e.progress(taskList(lines, -1), -1, len(indexes), 0, "", "", "complete")
	return config.SaveState(e.Root, state)
}

func (e *Engine) wait(ctx context.Context) error {
	if e.Control == nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	return e.Control.Wait(ctx)
}

func (e *Engine) maxRetries() int {
	return e.Config.EffectiveMaxRetries()
}

func (e *Engine) progress(tasks []Task, current, ordinal, attempt int, role, cli, verdict string) {
	if e.Update == nil {
		return
	}
	copyTasks := append([]Task(nil), tasks...)
	if current >= 0 && current < len(copyTasks) && copyTasks[current].Status != "done" {
		copyTasks[current].Status = "current"
	}
	e.Update(Progress{Project: e.Config.ProjectName, Tasks: copyTasks, CurrentTask: ordinal, TotalTasks: len(copyTasks), Attempt: attempt, MaxRetries: e.maxRetries(), Role: role, CLI: cli, RoleAgents: e.Config.Agents, Verdict: verdict, Paused: e.Control != nil && e.Control.IsPaused()})
}

func (e *Engine) prompt(role, task string) (string, error) {
	read := func(n string) string { b, _ := os.ReadFile(filepath.Join(e.Root, config.Dir, n)); return string(b) }
	rolePrompt := read("roles/" + role + ".md")
	rules := limitText(read("GOLDEN_RULES.md"), 10000)
	summary := limitText(read("state/context_summary.md"), 6000)
	feedback := limitText(read("state/feedback.md"), 10000)
	contextText := limitText(read("CONTEXT.md"), 12000)
	if summary == "" {
		summary = "No previous task summary."
	}
	switch role {
	case "reviewer":
		contextText = "Use the current diff, task, tests, summary, and relevant rules. Do not reconstruct the whole project."
		rules = limitText(rules, 4000)
	case "judge":
		contextText = "Resolve only the reviewer disagreement using the task evidence and current diff."
		rules = limitText(rules, 2500)
	}
	prompt := fmt.Sprintf("ROLE:\n%s\nCONTEXT:\n%s\nRULES:\n%s\nTASK:\n%s\nSUMMARY:\n%s\nFEEDBACK:\n%s", rolePrompt, contextText, rules, task, summary, feedback)
	budget := e.budget(role)
	maxChars := budget.MaxInputChars
	if budget.MaxInputTokens > 0 && budget.MaxInputTokens*4 < maxChars {
		maxChars = budget.MaxInputTokens * 4
	}
	return limitText(prompt, maxChars), nil
}

func (e *Engine) budget(role string) config.RoleBudget { return e.Config.Budget(role) }

func (e *Engine) limitRoleInput(role, value string) string {
	budget := e.budget(role)
	maxChars := budget.MaxInputChars
	if budget.MaxInputTokens > 0 && budget.MaxInputTokens*4 < maxChars {
		maxChars = budget.MaxInputTokens * 4
	}
	return limitText(value, maxChars)
}

func (e *Engine) runRole(ctx context.Context, role, prompt string, turns map[string]int, metrics map[string]history.AgentMetric) (string, error) {
	budget := e.budget(role)
	turns[role]++
	if budget.MaxTurns > 0 && turns[role] > budget.MaxTurns {
		return "", fmt.Errorf("%s budget exhausted: maximum %d turns reached", role, budget.MaxTurns)
	}
	started := time.Now()
	roleCtx := agent.WithEffort(ctx, budget.Effort)
	if e.Config.ReuseSessions {
		roleCtx = agent.WithSession(roleCtx, e.Sessions[role])
		roleCtx = agent.WithSessionTemplate(roleCtx, e.Config.Commands[e.Config.Agents[role]].SessionArgs)
	}
	rawOutput, err := e.runAgent(roleCtx, role, prompt)
	maxOutputChars := budget.MaxOutputChars
	if budget.MaxOutputTokens > 0 && budget.MaxOutputTokens*4 < maxOutputChars {
		maxOutputChars = budget.MaxOutputTokens * 4
	}
	output := limitOutput(rawOutput, maxOutputChars)
	if e.Config.ReuseSessions {
		if session := extractSessionID(output); session != "" {
			e.Sessions[role] = session
		}
	}
	key := role
	if turns[role] > 1 {
		key = fmt.Sprintf("%s#%d", role, turns[role])
	}
	status := "completed"
	if err != nil {
		status = "failed"
		message := fmt.Sprintf("%s failed: %v\n\nRecovery: inspect the last output, correct the agent configuration or quota issue, then rerun the pending task.", role, err)
		_ = os.WriteFile(filepath.Join(e.Root, config.Dir, "state", "feedback.md"), []byte(message), 0644)
		e.emit("✗ " + message)
	}
	inputTokens, outputTokens := parseTokenMetrics(rawOutput)
	metrics[key] = history.AgentMetric{Role: role, CLI: e.Config.Agents[role], Model: e.Config.Models[role], Effort: budget.Effort, Turns: turns[role], InputChars: len(prompt), OutputChars: len(rawOutput), InputTokens: inputTokens, OutputTokens: outputTokens, EstimatedTokens: estimateTokens(prompt) + estimateTokens(rawOutput), Duration: time.Since(started), Status: status, Retryable: retryableError(err), Error: errorText(err), SessionID: e.Sessions[role]}
	return output, err
}

var sessionIDRE = regexp.MustCompile(`(?i)(?:session[_ -]?id|sessionId|thread[_ -]?id|conversation[_ -]?id)\s*[=:]\s*["']?([a-z0-9-]{16,})`)

var tokenMetricRE = regexp.MustCompile(`(?i)(input|prompt|output|completion)?\s*tokens?\s*(?:used|:|=)\s*([0-9,]+)`)

func extractSessionID(output string) string {
	match := sessionIDRE.FindStringSubmatch(output)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

func parseTokenMetrics(output string) (input, outputTokens int) {
	for _, match := range tokenMetricRE.FindAllStringSubmatch(output, -1) {
		if len(match) != 3 {
			continue
		}
		n, _ := strconv.Atoi(strings.ReplaceAll(match[2], ",", ""))
		switch strings.ToLower(match[1]) {
		case "input", "prompt":
			input += n
		case "output", "completion":
			outputTokens += n
		default:
			outputTokens += n
		}
	}
	return input, outputTokens
}

func estimateTokens(value string) int {
	if strings.TrimSpace(value) == "" {
		return 0
	}
	return (len(value) + 3) / 4
}

func retryableError(err error) bool {
	if err == nil {
		return false
	}
	return retryableText(err.Error())
}

func retryableText(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"429", "rate limit", "rate_limit", "quota", "capacity", "too many requests", "temporarily unavailable", "timeout", "timed out", "connection reset", "resource exhausted", "usage limit", "context window exceeded"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func summarizeAgentError(value string) string {
	words := strings.Fields(value)
	if len(words) > 32 {
		words = words[len(words)-32:]
	}
	return limitText(strings.Join(words, " "), 500)
}

func waitBackoff(ctx context.Context, seconds int) error {
	if seconds <= 0 {
		return nil
	}
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func writeContextSummary(root, task string, metrics map[string]history.AgentMetric) error {
	var b strings.Builder
	b.WriteString("# Incremental task summary\n\n")
	b.WriteString("Completed task: " + task + "\n\n")
	b.WriteString("Files: " + strings.Join(gitutil.ChangedFiles(root), ", ") + "\n")
	b.WriteString("Agents:\n")
	for name, metric := range metrics {
		fmt.Fprintf(&b, "- %s: %s, %d input chars, %d output chars, %s\n", name, metric.Status, metric.InputChars, metric.OutputChars, metric.Duration.Round(time.Millisecond))
	}
	testOutput, _ := os.ReadFile(filepath.Join(root, config.Dir, "state", "test_output.log"))
	if len(testOutput) > 3000 {
		testOutput = testOutput[len(testOutput)-3000:]
	}
	b.WriteString("\nLatest validation output:\n```\n")
	b.Write(testOutput)
	b.WriteString("\n```\n")
	return os.WriteFile(filepath.Join(root, config.Dir, "state", "context_summary.md"), []byte(b.String()), 0644)
}

func limitText(value string, max int) string {
	value = normalizeUTF8(value)
	if max <= 0 || len(value) <= max {
		return value
	}
	marker := "\n\n[PAOLA: context truncated to respect role budget]\n"
	if len(marker) >= max {
		return utf8Prefix(value, max)
	}
	return utf8Prefix(value, max-len(marker)) + marker
}

func limitOutput(value string, max int) string {
	value = normalizeUTF8(value)
	if max <= 0 || len(value) <= max {
		return value
	}
	marker := "\n\n[PAOLA: agent output truncated to respect role budget]\n\n"
	if max <= len(marker)+2 {
		return utf8Prefix(value, max)
	}
	remaining := max - len(marker)
	head := remaining * 3 / 4
	tail := remaining - head
	return utf8Prefix(value, head) + marker + utf8Suffix(value, tail)
}

func normalizeUTF8(value string) string {
	if utf8.ValidString(value) {
		return value
	}
	return strings.ToValidUTF8(value, "�")
}

func utf8Prefix(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	maxBytes = minInt(maxBytes, len(value))
	for maxBytes > 0 && !utf8.ValidString(value[:maxBytes]) {
		maxBytes--
	}
	return value[:maxBytes]
}

func utf8Suffix(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	start := len(value) - maxBytes
	for start < len(value) && !utf8.ValidString(value[start:]) {
		start++
	}
	return value[start:]
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func (e *Engine) runAgent(ctx context.Context, role, prompt string) (string, error) {
	cli := e.Config.Agents[role]
	commands := e.Config.Commands[cli]
	template := commands.Coder
	if role == "reviewer" {
		template = commands.Reviewer
	}
	if role == "judge" {
		template = commands.Judge
	}
	if e.Config.Simulation {
		switch role {
		case "reviewer":
			return "SIMULATION\nSTATUS: LGTM", nil
		case "judge":
			return "SIMULATION\nVERDICT: OVERRIDE_APPROVE", nil
		default:
			return "SIMULATION\nNo agent invoked and no files changed.", nil
		}
	}
	timeoutMinutes := e.Config.EffectiveIdleTimeoutMinutes()
	ctx, cancel := context.WithCancel(agent.WithIdleTimeout(ctx, time.Duration(timeoutMinutes)*time.Minute))
	defer cancel()
	logs := make(chan string, 256)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for line := range logs {
			// Keep the complete agent transcript in encrypted history, but avoid
			// flooding the live TUI with shell plumbing and macOS xcrun cache
			// diagnostics that do not describe the task's actual progress.
			if shouldShowLiveLog(line) {
				e.emit(line)
			}
		}
	}()
	result, err := e.Runner.Run(ctx, e.Root, cli, template, prompt, e.Config.Models[role], logs)
	close(logs)
	<-done
	if err == nil && retryableText(result) {
		return result, fmt.Errorf("%s agent (%s) reported a retryable quota or capacity failure: %s", role, cli, summarizeAgentError(result))
	}
	if err != nil {
		return result, fmt.Errorf("%s agent (%s) failed: %w", role, cli, err)
	}
	return result, err
}

func shouldShowLiveLog(line string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "exec /bin/") && strings.Contains(trimmed, " git ") {
		return false
	}
	if strings.HasPrefix(trimmed, "succeeded in ") {
		return false
	}
	if strings.Contains(trimmed, "DARWIN_USER_TEMP_DIR") ||
		strings.Contains(trimmed, "xcrun_db-") ||
		strings.Contains(trimmed, "couldn't create cache file") {
		return false
	}
	return true
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func completeTask(root, roadmapPath string, lines []string, idx int, task string, state config.State) error {
	lines[idx] = strings.Replace(lines[idx], "- [ ]", "- [x]", 1)
	if err := os.WriteFile(roadmapPath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		return err
	}
	if gitutil.IsRepo(root) {
		_ = gitutil.Commit(root, task)
	}
	state.History = append(state.History, "completed: "+task)
	state.CurrentTask++
	state.Attempt = 0
	return config.SaveState(root, state)
}

func runTest(ctx context.Context, root, command string) (string, error) {
	c := exec.CommandContext(ctx, "sh", "-c", command)
	c.Dir = root
	b, err := c.CombinedOutput()
	return string(b), err
}

func hasMarker(output, marker string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == marker {
			return true
		}
	}
	return strings.Contains(output, marker)
}

func tokenCountFromFiles(root string) int {
	re := regexp.MustCompile(`(?i)tokens?\s+used\s+([0-9,]+)`)
	total := 0
	characters := 0
	for _, name := range []string{"current_prompt.md", "current.diff", "feedback.md"} {
		b, _ := os.ReadFile(filepath.Join(root, config.Dir, "state", name))
		text := string(b)
		characters += len(text)
		for _, match := range re.FindAllStringSubmatch(text, -1) {
			if len(match) == 2 {
				n, _ := strconv.Atoi(strings.ReplaceAll(match[1], ",", ""))
				total += n
			}
		}
	}
	if total == 0 && characters > 0 {
		return (characters + 3) / 4
	}
	return total
}

func taskTokenCount(root string, metrics map[string]history.AgentMetric) int {
	total := 0
	for _, metric := range metrics {
		if metric.InputTokens+metric.OutputTokens > 0 {
			total += metric.InputTokens + metric.OutputTokens
			continue
		}
		total += metric.EstimatedTokens
	}
	if total > 0 {
		return total
	}
	return tokenCountFromFiles(root)
}

func pendingTaskIndexes(lines []string) []int {
	var indexes []int
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "- [ ]") {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

func taskList(lines []string, current int) []Task {
	var tasks []Task
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		status := ""
		switch {
		case strings.HasPrefix(trimmed, "- [x]") || strings.HasPrefix(trimmed, "- [X]"):
			status = "done"
		case strings.HasPrefix(trimmed, "- [ ]"):
			status = "pending"
		default:
			continue
		}
		if i == current && status != "done" {
			status = "current"
		}
		tasks = append(tasks, Task{Description: gitutil.CleanTask(trimmed), Status: status})
	}
	return tasks
}
