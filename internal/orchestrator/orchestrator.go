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
	defer func() { _ = record.FinishWithError(runStatus, errorText(runErr)) }()
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
			e.emit(fmt.Sprintf("Coder attempt %d/%d (%s)", state.Attempt, e.Config.MaxRetries, coder))
			coderOutput, err := e.runAgent(ctx, "coder", prompt)
			_ = record.Log(ordinal+1, "coder", coderOutput)
			if err != nil {
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
					if state.Attempt >= e.Config.MaxRetries {
						return fmt.Errorf("tests failed after %d attempts", state.Attempt)
					}
					continue
				}
			}

			diff, err := gitutil.CaptureDiff(e.Root)
			if err != nil {
				return fmt.Errorf("capture Git diff for task %q: %w", task, err)
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
			review, err := e.runAgent(ctx, "reviewer", reviewPrompt+"\nCURRENT DIFF:\n"+diff)
			_ = record.Log(ordinal+1, "reviewer", review)
			if err != nil {
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
			if state.Attempt < e.Config.MaxRetries {
				e.emit("Reviewer requested changes")
				continue
			}

			judge := e.Config.Agents["judge"]
			e.progress(taskList(lines, idx), -1, ordinal+1, state.Attempt, "Judge", judge, "")
			judgePrompt, _ := e.prompt("judge", task)
			e.emit("Judge (" + judge + ")")
			verdict, err := e.runAgent(ctx, "judge", judgePrompt+"\nREVIEW:\n"+review+"\nCURRENT DIFF:\n"+diff)
			_ = record.Log(ordinal+1, "judge", verdict)
			if err != nil {
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
			bonusOutput, err := e.runAgent(ctx, "coder", coderPrompt)
			_ = record.Log(ordinal+1, "coder-bonus", bonusOutput)
			if err != nil {
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
			_ = record.UpdateTask(ordinal+1, "completed", "", tokenCountFromFiles(e.Root), gitutil.ChangedFiles(e.Root), state.Attempt, reviewRounds)
			_ = os.Remove(promptPath)
			if err := completeTask(e.Root, roadmapPath, lines, idx, task, state); err != nil {
				return err
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

func (e *Engine) progress(tasks []Task, current, ordinal, attempt int, role, cli, verdict string) {
	if e.Update == nil {
		return
	}
	copyTasks := append([]Task(nil), tasks...)
	if current >= 0 && current < len(copyTasks) && copyTasks[current].Status != "done" {
		copyTasks[current].Status = "current"
	}
	e.Update(Progress{Project: e.Config.ProjectName, Tasks: copyTasks, CurrentTask: ordinal, TotalTasks: len(copyTasks), Attempt: attempt, MaxRetries: e.Config.MaxRetries, Role: role, CLI: cli, RoleAgents: e.Config.Agents, Verdict: verdict, Paused: e.Control != nil && e.Control.IsPaused()})
}

func (e *Engine) prompt(role, task string) (string, error) {
	read := func(n string) string { b, _ := os.ReadFile(filepath.Join(e.Root, config.Dir, n)); return string(b) }
	return fmt.Sprintf("ROLE:\n%s\nCONTEXT:\n%s\nRULES:\n%s\nTASK:\n%s\nFEEDBACK:\n%s", read("roles/"+role+".md"), read("CONTEXT.md"), read("GOLDEN_RULES.md"), task, read("state/feedback.md")), nil
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
	logs := make(chan string)
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
