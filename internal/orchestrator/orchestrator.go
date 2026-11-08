package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
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
	Root        string
	Config      config.Config
	Runner      agent.AgentRunner
	Log         LogFunc
	Update      ProgressFunc
	Control     *Control
	TaskBranch  string
	BaseBranch  string
	ExecutionID string
	Sessions    map[string]string
}

type taskCheckpoint struct {
	Task        string            `json:"task"`
	TaskIndex   int               `json:"task_index"`
	Attempt     int               `json:"attempt"`
	Stage       string            `json:"stage"`
	Status      string            `json:"status"`
	NextAction  string            `json:"next_action,omitempty"`
	Files       []string          `json:"files,omitempty"`
	TestStatus  string            `json:"test_status,omitempty"`
	SessionKeys map[string]string `json:"session_keys,omitempty"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

func checkpointIsContinuation(raw string) bool {
	var cp taskCheckpoint
	if json.Unmarshal([]byte(raw), &cp) != nil {
		return false
	}
	// The first coder turn is prepared with a checkpoint too, but it must
	// receive the complete project context. Every later stage/attempt can use
	// the compact checkpoint packet instead.
	return cp.Attempt > 1 || (cp.Stage != "" && cp.Stage != "coder")
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
	previousExecution := state.ExecutionID
	if state.Status == "complete" {
		previousExecution = ""
	}
	if e.Sessions == nil {
		e.Sessions = state.Sessions
	}
	if e.Sessions == nil {
		e.Sessions = map[string]string{}
	}
	// A completed workspace starts a new execution namespace. Do not carry
	// provider sessions from an older run into it; those sessions may be stale
	// and can contain unrelated task context.
	if previousExecution == "" {
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
	if previousExecution != "" {
		e.ExecutionID = previousExecution
		if err := record.SetParentExecution(previousExecution); err != nil {
			return err
		}
	} else {
		e.ExecutionID = record.ID()
	}
	state.ExecutionID = e.ExecutionID
	if err := record.Event(history.Event{Type: "execution_started", Status: "running"}); err != nil {
		return err
	}
	runStatus := "failed"
	activeTaskIndex := 0
	activeTask := ""
	activeTaskFinished := false
	promptPath := filepath.Join(e.Root, config.Dir, "state", "current_prompt.md")
	_ = os.Remove(promptPath)
	defer func() { _ = os.Remove(promptPath) }()
	defer func() {
		var finalizeErr error
		state.Sessions = e.Sessions
		finalizeErr = errors.Join(finalizeErr, config.SaveState(e.Root, state))
		if runErr != nil && ctx.Err() != nil {
			runStatus = "interrupted"
			state.Status = "interrupted"
			finalizeErr = errors.Join(finalizeErr, config.SaveState(e.Root, state))
		}
		if runErr != nil && activeTaskIndex > 0 && !activeTaskFinished {
			finalizeErr = errors.Join(finalizeErr, record.Event(history.Event{TaskIndex: activeTaskIndex, Task: activeTask, Type: "task_finished", Status: "failed", Error: summarizeAgentError(errorText(runErr))}))
		}
		status := runStatus
		if runErr != nil && status == "failed" && ctx.Err() != nil {
			status = "interrupted"
		}
		finalizeErr = errors.Join(finalizeErr, record.Event(history.Event{Type: "execution_finished", Status: status, Error: auditError(runErr)}))
		finalizeErr = errors.Join(finalizeErr, record.FinishWithError(status, errorText(runErr)))
		if finalizeErr != nil {
			if runErr == nil {
				runErr = fmt.Errorf("history finalization failed: %w", finalizeErr)
			} else {
				runErr = errors.Join(runErr, finalizeErr)
			}
		}
	}()
	if state.Status == "" {
		state.Status = "running"
	}
	for ordinal, idx := range indexes {
		task := gitutil.CleanTask(lines[idx])
		activeTaskIndex = ordinal + 1
		activeTask = task
		activeTaskFinished = false
		_ = os.Remove(promptPath)
		startedAt := time.Now().UTC()
		if err := record.RecordTask(history.TaskRecord{Index: ordinal + 1, Description: task, Status: "running", StartedAt: startedAt}); err != nil {
			return err
		}
		if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Type: "task_started", Status: "running"}); err != nil {
			return err
		}
		state.CurrentTask = ordinal
		state.Attempt = 0
		taskBase, _ := gitutil.Head(e.Root)
		taskKey := fmt.Sprintf("task-%03d", ordinal+1)
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
		testStatus := "not_run"
		metrics := map[string]history.AgentMetric{}
		turns := map[string]int{}
		for !completed {
			if err := e.wait(ctx); err != nil {
				return err
			}
			if e.Control != nil && e.Control.ConsumeSkip() {
				e.emit("↷ skipped: " + task)
				if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Type: "task_finished", Status: "skipped"}); err != nil {
					return err
				}
				activeTaskFinished = true
				break
			}
			state.Attempt++
			if err := config.SaveState(e.Root, state); err != nil {
				return err
			}
			coder := e.Config.Agents["coder"]
			e.progress(taskList(lines, idx), -1, ordinal+1, state.Attempt, "Coder", coder, "")
			if err := e.saveCheckpoint(taskCheckpoint{Task: task, TaskIndex: ordinal + 1, Attempt: state.Attempt, Stage: "coder", Status: "running", NextAction: "coder turn", SessionKeys: e.taskSessions(taskKey)}); err != nil {
				return err
			}
			prompt, _ := e.prompt("coder", task, taskKey)
			_ = os.WriteFile(filepath.Join(e.Root, config.Dir, "state", "current_prompt.md"), []byte(prompt), 0600)
			e.emit(fmt.Sprintf("Coder attempt %d/%d (%s)", state.Attempt, maxRetries, coder))
			if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Role: "coder", Agent: coder, Type: "agent_started", Status: "running"}); err != nil {
				return err
			}
			coderOutput, err := e.runRole(ctx, "coder", prompt, taskKey, turns, metrics)
			if saveErr := e.saveSessionState(); saveErr != nil {
				return fmt.Errorf("persist session state: %w", saveErr)
			}
			if logErr := record.LogAttempt(ordinal+1, state.Attempt, "coder", coderOutput); logErr != nil {
				return fmt.Errorf("persist coder transcript: %w", logErr)
			}
			coderStatus := "completed"
			if err != nil {
				coderStatus = "failed"
			}
			if eventErr := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Role: "coder", Agent: coder, Type: "agent_finished", Status: coderStatus, Error: auditError(err), OutputRef: fmt.Sprintf("logs/tasks/%03d-attempt-%02d-coder.enc", ordinal+1, state.Attempt)}); eventErr != nil {
				return eventErr
			}
			if err != nil {
				if metricErr := record.UpdateTaskMetrics(ordinal+1, metrics); metricErr != nil {
					return metricErr
				}
				if retryableError(err) && state.Attempt < maxRetries {
					if eventErr := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Role: "coder", Agent: coder, Type: "retry_scheduled", Status: "retrying", Error: auditError(err)}); eventErr != nil {
						return eventErr
					}
					e.emit(fmt.Sprintf("Retryable coder failure; retrying in %ds", e.Config.EffectiveRetryBackoffSeconds()))
					if err := waitBackoff(ctx, e.Config.EffectiveRetryBackoffSeconds()); err != nil {
						return err
					}
					continue
				}
				return err
			}

			if e.Config.TestCommand != "" {
				if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Type: "tests_started", Status: "running"}); err != nil {
					return err
				}
				if err := e.saveCheckpoint(taskCheckpoint{Task: task, TaskIndex: ordinal + 1, Attempt: state.Attempt, Stage: "tests", Status: "running", NextAction: "validation", SessionKeys: e.taskSessions(taskKey)}); err != nil {
					return err
				}
				testResult := runTestDetailed(ctx, e.Root, e.Config.TestCommand)
				testOut, testErr := testResult.Output, testResult.Err
				if err := os.WriteFile(filepath.Join(e.Root, config.Dir, "state", "test_output.log"), []byte(testOut), 0644); err != nil {
					return err
				}
				testRef, evidenceErr := record.LogTestEvidence(ordinal+1, state.Attempt, history.TestEvidence{Command: e.Config.TestCommand, Workdir: e.Root, StartedAt: testResult.StartedAt, FinishedAt: testResult.FinishedAt, Duration: testResult.FinishedAt.Sub(testResult.StartedAt), ExitCode: testResult.ExitCode, Status: statusFromError(testErr), Output: testOut})
				if evidenceErr != nil {
					return fmt.Errorf("persist test evidence: %w", evidenceErr)
				}
				if testErr != nil {
					if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Type: "tests_finished", Status: "failed", Error: auditError(testErr), Evidence: []string{testRef}}); err != nil {
						return err
					}
					testStatus = "failed"
					if err := e.saveCheckpoint(taskCheckpoint{Task: task, TaskIndex: ordinal + 1, Attempt: state.Attempt, Stage: "tests", Status: "failed", TestStatus: testStatus, NextAction: "coder correction", SessionKeys: e.taskSessions(taskKey)}); err != nil {
						return err
					}
					if err := os.WriteFile(filepath.Join(e.Root, config.Dir, "state", "feedback.md"), []byte(testOut), 0644); err != nil {
						return err
					}
					e.emit("Tests failed; returning to Coder")
					if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Type: "retry_scheduled", Status: "retrying", Error: auditError(testErr), Evidence: []string{testRef}}); err != nil {
						return err
					}
					if summaryErr := writeContextSummary(e.Root, task, metrics); summaryErr != nil {
						return summaryErr
					}
					if state.Attempt >= maxRetries {
						return fmt.Errorf("tests failed after %d attempts", state.Attempt)
					}
					continue
				}
				testStatus = "passed"
				if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Type: "tests_finished", Status: "passed", Evidence: []string{testRef}}); err != nil {
					return err
				}
				if err := e.saveCheckpoint(taskCheckpoint{Task: task, TaskIndex: ordinal + 1, Attempt: state.Attempt, Stage: "tests", Status: "completed", TestStatus: testStatus, NextAction: "review diff", SessionKeys: e.taskSessions(taskKey)}); err != nil {
					return err
				}
			}

			diff := ""
			if e.Config.Simulation {
				diff = "SIMULATION: no project diff captured.\n"
			} else {
				var diffErr error
				diff, diffErr = gitutil.CaptureDiffSince(e.Root, taskBase)
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
			if err := record.DiffAttempt(ordinal+1, state.Attempt, diff); err != nil {
				return fmt.Errorf("persist task %d attempt %d diff: %w", ordinal+1, state.Attempt, err)
			}
			reviewer := e.Config.Agents["reviewer"]
			reviewRounds++
			e.progress(taskList(lines, idx), -1, ordinal+1, state.Attempt, "Reviewer", reviewer, "")
			if err := e.saveCheckpoint(taskCheckpoint{Task: task, TaskIndex: ordinal + 1, Attempt: state.Attempt, Stage: "reviewer", Status: "running", TestStatus: testStatus, NextAction: "review diff", Files: gitutil.ChangedFilesSince(e.Root, taskBase), SessionKeys: e.taskSessions(taskKey)}); err != nil {
				return err
			}
			reviewPrompt, _ := e.prompt("reviewer", task, taskKey)
			e.emit("Reviewer (" + reviewer + ")")
			if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Role: "reviewer", Agent: reviewer, Type: "agent_started", Status: "running", Evidence: []string{fmt.Sprintf("diffs/task-%03d-attempt-%02d.patch.enc", ordinal+1, state.Attempt)}}); err != nil {
				return err
			}
			reviewInput := e.limitRoleInput("reviewer", reviewPrompt+"\nCURRENT DIFF:\n"+diff)
			review, err := e.runRole(ctx, "reviewer", reviewInput, taskKey, turns, metrics)
			if saveErr := e.saveSessionState(); saveErr != nil {
				return fmt.Errorf("persist session state: %w", saveErr)
			}
			if logErr := record.LogAttempt(ordinal+1, state.Attempt, "reviewer", review); logErr != nil {
				return fmt.Errorf("persist reviewer transcript: %w", logErr)
			}
			reviewerStatus := "completed"
			if err != nil {
				reviewerStatus = "failed"
			}
			if eventErr := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Role: "reviewer", Agent: reviewer, Type: "agent_finished", Status: reviewerStatus, Error: auditError(err), OutputRef: fmt.Sprintf("logs/tasks/%03d-attempt-%02d-reviewer.enc", ordinal+1, state.Attempt)}); eventErr != nil {
				return eventErr
			}
			if err != nil {
				if metricErr := record.UpdateTaskMetrics(ordinal+1, metrics); metricErr != nil {
					return metricErr
				}
				if retryableError(err) && state.Attempt < maxRetries {
					if eventErr := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Role: "reviewer", Agent: reviewer, Type: "retry_scheduled", Status: "retrying", Error: auditError(err)}); eventErr != nil {
						return eventErr
					}
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
			decisionStatus := "changes_requested"
			decisionRecorded, decisionErr := parseReviewerDecision(review)
			if decisionErr != nil {
				return decisionErr
			}
			if decisionRecorded == "STATUS: LGTM" {
				decisionStatus = "approved"
			}
			if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Role: "reviewer", Agent: reviewer, Type: "review_decision", Status: decisionStatus, Decision: decisionRecorded}); err != nil {
				return err
			}
			if decisionStatus == "approved" {
				completed = true
				e.emit("✓ reviewer approved")
				break
			}
			if state.Attempt < maxRetries {
				if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Type: "retry_scheduled", Status: "retrying", Error: "reviewer requested changes"}); err != nil {
					return err
				}
				e.emit("Reviewer requested changes")
				if summaryErr := writeContextSummary(e.Root, task, metrics); summaryErr != nil {
					return summaryErr
				}
				continue
			}

			judge := e.Config.Agents["judge"]
			e.progress(taskList(lines, idx), -1, ordinal+1, state.Attempt, "Judge", judge, "")
			if err := e.saveCheckpoint(taskCheckpoint{Task: task, TaskIndex: ordinal + 1, Attempt: state.Attempt, Stage: "judge", Status: "running", NextAction: "resolve review disagreement", Files: gitutil.ChangedFilesSince(e.Root, taskBase), SessionKeys: e.taskSessions(taskKey)}); err != nil {
				return err
			}
			judgePrompt, _ := e.prompt("judge", task, taskKey)
			e.emit("Judge (" + judge + ")")
			if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Role: "judge", Agent: judge, Type: "agent_started", Status: "running"}); err != nil {
				return err
			}
			judgeInput := e.limitRoleInput("judge", judgePrompt+"\nREVIEW:\n"+review+"\nCURRENT DIFF:\n"+diff)
			verdict, err := e.runRole(ctx, "judge", judgeInput, taskKey, turns, metrics)
			if saveErr := e.saveSessionState(); saveErr != nil {
				return fmt.Errorf("persist session state: %w", saveErr)
			}
			if logErr := record.LogAttempt(ordinal+1, state.Attempt, "judge", verdict); logErr != nil {
				return fmt.Errorf("persist judge transcript: %w", logErr)
			}
			judgeStatus := "completed"
			if err != nil {
				judgeStatus = "failed"
			}
			if eventErr := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Role: "judge", Agent: judge, Type: "agent_finished", Status: judgeStatus, Error: auditError(err), OutputRef: fmt.Sprintf("logs/tasks/%03d-attempt-%02d-judge.enc", ordinal+1, state.Attempt)}); eventErr != nil {
				return eventErr
			}
			if err != nil {
				if metricErr := record.UpdateTaskMetrics(ordinal+1, metrics); metricErr != nil {
					return metricErr
				}
				return err
			}
			if err := os.WriteFile(filepath.Join(e.Root, config.Dir, "state", "feedback.md"), []byte(verdict), 0644); err != nil {
				return err
			}
			judgeDecision, decisionErr := parseJudgeDecision(verdict)
			if decisionErr != nil {
				return decisionErr
			}
			if judgeDecision == "VERDICT: OVERRIDE_APPROVE" {
				if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Role: "judge", Agent: judge, Type: "judge_decision", Status: "approved", Decision: "VERDICT: OVERRIDE_APPROVE"}); err != nil {
					return err
				}
				completed = true
				e.emit("✓ judge approved")
				break
			}
			if judgeDecision != "VERDICT: FINAL_INSTRUCTIONS" {
				return fmt.Errorf("judge returned no recognized verdict for task %q", task)
			}
			if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Role: "judge", Agent: judge, Type: "judge_decision", Status: "final_instructions", Decision: "VERDICT: FINAL_INSTRUCTIONS"}); err != nil {
				return err
			}
			// The Judge's synthesis becomes feedback for exactly one final Coder turn.
			e.emit("Judge issued final instructions; starting bonus Coder turn")
			if err := e.wait(ctx); err != nil {
				return err
			}
			state.Attempt++
			coderPrompt, _ := e.prompt("coder", task, taskKey)
			e.progress(taskList(lines, idx), -1, ordinal+1, state.Attempt, "Coder (bonus)", coder, "FINAL_INSTRUCTIONS")
			if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Role: "coder", Agent: coder, Type: "agent_started", Status: "running"}); err != nil {
				return err
			}
			bonusOutput, err := e.runRole(ctx, "coder", coderPrompt, taskKey, turns, metrics)
			if saveErr := e.saveSessionState(); saveErr != nil {
				return fmt.Errorf("persist session state: %w", saveErr)
			}
			if logErr := record.LogAttempt(ordinal+1, state.Attempt, "coder-bonus", bonusOutput); logErr != nil {
				return fmt.Errorf("persist bonus coder transcript: %w", logErr)
			}
			if eventErr := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Role: "coder", Agent: coder, Type: "bonus_coder_finished", Status: statusFromError(err), Error: auditError(err), OutputRef: fmt.Sprintf("logs/tasks/%03d-attempt-%02d-coder-bonus.enc", ordinal+1, state.Attempt)}); eventErr != nil {
				return eventErr
			}
			if err != nil {
				if metricErr := record.UpdateTaskMetrics(ordinal+1, metrics); metricErr != nil {
					return metricErr
				}
				return fmt.Errorf("final coder turn failed: %w", err)
			}
			if e.Config.TestCommand != "" {
				if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Type: "tests_started", Status: "running"}); err != nil {
					return err
				}
				testResult := runTestDetailed(ctx, e.Root, e.Config.TestCommand)
				testOut, testErr := testResult.Output, testResult.Err
				if err := os.WriteFile(filepath.Join(e.Root, config.Dir, "state", "test_output.log"), []byte(testOut), 0644); err != nil {
					return err
				}
				testRef, evidenceErr := record.LogTestEvidence(ordinal+1, state.Attempt, history.TestEvidence{Command: e.Config.TestCommand, Workdir: e.Root, StartedAt: testResult.StartedAt, FinishedAt: testResult.FinishedAt, Duration: testResult.FinishedAt.Sub(testResult.StartedAt), ExitCode: testResult.ExitCode, Status: statusFromError(testErr), Output: testOut})
				if evidenceErr != nil {
					return fmt.Errorf("persist final test evidence: %w", evidenceErr)
				}
				if testErr != nil {
					if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Type: "tests_finished", Status: "failed", Error: auditError(testErr), Evidence: []string{testRef}}); err != nil {
						return err
					}
					if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Type: "task_finished", Status: "failed", Error: auditError(testErr), Evidence: []string{testRef}}); err != nil {
						return err
					}
					return fmt.Errorf("final coder tests failed: %w", testErr)
				}
				if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Type: "tests_finished", Status: "passed", Evidence: []string{testRef}}); err != nil {
					return err
				}
			}
			finalDiff, err := gitutil.CaptureDiffSince(e.Root, taskBase)
			if err != nil {
				return err
			}
			if err := os.WriteFile(gitutil.SafeDiffPath(e.Root), []byte(finalDiff), 0644); err != nil {
				return err
			}
			if err := record.DiffAttempt(ordinal+1, state.Attempt, finalDiff); err != nil {
				return fmt.Errorf("persist final task %d attempt %d diff: %w", ordinal+1, state.Attempt, err)
			}
			completed = true
			e.emit("✓ final coder turn complete")
		}
		if completed {
			if err := record.UpdateTask(ordinal+1, "completed", "", taskTokenCount(e.Root, metrics), gitutil.ChangedFilesSince(e.Root, taskBase), state.Attempt, reviewRounds); err != nil {
				return err
			}
			if err := record.UpdateTaskMetrics(ordinal+1, metrics); err != nil {
				return err
			}
			if err := record.Event(history.Event{TaskIndex: ordinal + 1, Task: task, Attempt: state.Attempt, Type: "task_finished", Status: "completed", Evidence: gitutil.ChangedFilesSince(e.Root, taskBase)}); err != nil {
				return err
			}
			activeTaskFinished = true
			if summaryErr := writeContextSummary(e.Root, task, metrics); summaryErr != nil {
				return summaryErr
			}
			if err := e.saveCheckpoint(taskCheckpoint{Task: task, TaskIndex: ordinal + 1, Attempt: state.Attempt, Stage: "completed", Status: "completed", NextAction: "next roadmap task", Files: gitutil.ChangedFilesSince(e.Root, taskBase), SessionKeys: e.taskSessions(taskKey)}); err != nil {
				return err
			}
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

func (e *Engine) prompt(role, task, taskKey string) (string, error) {
	read := func(n string) string { b, _ := os.ReadFile(filepath.Join(e.Root, config.Dir, n)); return string(b) }
	rolePrompt := read("roles/" + role + ".md")
	rules := limitText(read("GOLDEN_RULES.md"), 10000)
	summary := limitText(read("state/context_summary.md"), 6000)
	feedback := limitText(read("state/feedback.md"), 10000)
	contextText := limitText(read("CONTEXT.md"), 12000)
	checkpoint := e.readCheckpoint(taskKey)
	if summary == "" {
		summary = "No previous task summary."
	}
	// A resumed provider session already owns the static conversation context.
	// Send only the task delta and durable checkpoint instead of replaying the
	// entire project context on every retry. Providers without resume still get
	// the compact checkpoint and current evidence, but not historical logs.
	if e.providerSession(taskKey, role) != "" || checkpointIsContinuation(checkpoint) {
		rolePrompt = "Continue the existing task session. Preserve prior decisions unless new evidence invalidates them."
		contextText = "Use the existing session plus the task checkpoint. Do not reread unrelated history."
		rules = limitText(rules, 1500)
		feedback = limitText(feedback, 5000)
	}
	switch role {
	case "reviewer":
		contextText = "Use the current diff, task, tests, summary, and relevant rules. Do not reconstruct the whole project."
		rules = limitText(rules, 4000)
	case "judge":
		contextText = "Resolve only the reviewer disagreement using the task evidence and current diff."
		rules = limitText(rules, 2500)
	}
	prompt := fmt.Sprintf("ROLE:\n%s\nCONTEXT:\n%s\nRULES:\n%s\nTASK:\n%s\nSUMMARY:\n%s\nCHECKPOINT:\n%s\nFEEDBACK:\n%s", rolePrompt, contextText, rules, task, summary, checkpoint, feedback)
	budget := e.budget(role)
	maxChars := budget.MaxInputChars
	if budget.MaxInputTokens > 0 && budget.MaxInputTokens*4 < maxChars {
		maxChars = budget.MaxInputTokens * 4
	}
	protocol := roleProtocol(role)
	if protocol == "" {
		return limitText(prompt, maxChars), nil
	}
	// Keep the machine-readable contract at the end of the prompt. A plain
	// prefix truncation must never remove the only instruction that controls
	// orchestration state transitions.
	if maxChars > 0 && len(protocol) < maxChars {
		bodyLimit := maxChars - len(protocol)
		return limitText(prompt, bodyLimit) + protocol, nil
	}
	return protocol, nil
}

func roleProtocol(role string) string {
	switch role {
	case "reviewer":
		return "\n\nPAOLA PROTOCOL (machine-readable, mandatory):\nYour final non-empty line MUST be exactly one of:\nSTATUS: LGTM\nSTATUS: CHANGES_REQUESTED\nDo not replace it with APPROVED, PASS, or prose.\n"
	case "judge":
		return "\n\nPAOLA PROTOCOL (machine-readable, mandatory):\nYour final non-empty line MUST be exactly one of:\nVERDICT: OVERRIDE_APPROVE\nVERDICT: FINAL_INSTRUCTIONS\nDo not replace it with APPROVE, PASS, PILOT_BLOCKED, or prose.\n"
	default:
		return ""
	}
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

func (e *Engine) runRole(ctx context.Context, role, prompt, taskKey string, turns map[string]int, metrics map[string]history.AgentMetric) (string, error) {
	budget := e.budget(role)
	turns[role]++
	if budget.MaxTurns > 0 && turns[role] > budget.MaxTurns {
		return "", fmt.Errorf("%s budget exhausted: maximum %d turns reached", role, budget.MaxTurns)
	}
	started := time.Now()
	roleCtx := agent.WithEffort(ctx, budget.Effort)
	providerSessionID := ""
	contextMode := "checkpoint"
	sessionResumed := false
	fallbackUsed := false
	if e.Config.SessionsEnabled() {
		providerSessionID = e.Sessions[e.sessionKey(taskKey, role)]
		roleCtx = agent.WithSession(roleCtx, providerSessionID)
		sessionTemplate := e.sessionTemplate(e.Config.Agents[role])
		roleCtx = agent.WithSessionTemplate(roleCtx, sessionTemplate)
		if providerSessionID != "" && strings.TrimSpace(sessionTemplate) != "" {
			contextMode = "session_resume"
			sessionResumed = true
		}
	}
	rawOutput, err := e.runAgent(roleCtx, role, prompt)
	if err != nil && providerSessionID != "" && isSessionResumeFailure(err) {
		// Session IDs can expire or become incompatible after a CLI update. Do
		// not burn the whole task: start one clean provider turn using the
		// durable checkpoint already embedded in the compact prompt.
		e.emit(fmt.Sprintf("Session %s unavailable; starting a fresh %s turn from checkpoint", providerSessionID, role))
		freshCtx := agent.WithSessionTemplate(agent.WithSession(roleCtx, ""), "")
		rawOutput, err = e.runAgent(freshCtx, role, prompt)
		contextMode = "checkpoint_fallback"
		sessionResumed = false
		fallbackUsed = true
	}
	maxOutputChars := budget.MaxOutputChars
	if budget.MaxOutputTokens > 0 && budget.MaxOutputTokens*4 < maxOutputChars {
		maxOutputChars = budget.MaxOutputTokens * 4
	}
	// Keep the full provider transcript for encrypted audit history. Only the
	// bounded copy is used for session extraction and downstream prompts.
	output := limitOutput(rawOutput, maxOutputChars)
	if e.Config.SessionsEnabled() {
		if session := extractSessionID(output); session != "" {
			e.Sessions[e.sessionKey(taskKey, role)] = session
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
	metrics[key] = history.AgentMetric{Role: role, CLI: e.Config.Agents[role], Model: e.Config.Models[role], Effort: budget.Effort, Turns: turns[role], InputChars: len(prompt), OutputChars: len(rawOutput), InputTokens: inputTokens, OutputTokens: outputTokens, EstimatedTokens: estimateTokens(prompt) + estimateTokens(rawOutput), Duration: time.Since(started), Status: status, Retryable: retryableError(err), Error: errorText(err), SessionID: e.Sessions[e.sessionKey(taskKey, role)], ContextMode: contextMode, SessionResumed: sessionResumed, FallbackUsed: fallbackUsed}
	return rawOutput, err
}

func (e *Engine) sessionKey(taskKey, role string) string {
	return e.ExecutionID + "/" + taskKey + "/" + role
}

func (e *Engine) taskSessions(taskKey string) map[string]string {
	out := map[string]string{}
	for _, role := range []string{"coder", "reviewer", "judge"} {
		if id := e.Sessions[e.sessionKey(taskKey, role)]; id != "" {
			out[role] = id
		}
	}
	return out
}

func (e *Engine) providerSession(taskKey, role string) string {
	if !e.Config.SessionsEnabled() {
		return ""
	}
	cli := e.Config.Agents[role]
	if strings.TrimSpace(e.sessionTemplate(cli)) == "" {
		return ""
	}
	return e.Sessions[e.sessionKey(taskKey, role)]
}

func (e *Engine) sessionTemplate(cli string) string {
	if command := e.Config.Commands[cli]; strings.TrimSpace(command.SessionArgs) != "" {
		return command.SessionArgs
	}
	// Older workspaces do not have session_args in config.yaml. Resolve the
	// built-in adapter without mutating the user's faithfully loaded config.
	if command, ok := config.DefaultAgentCommand(cli); ok {
		return command.SessionArgs
	}
	return ""
}

func (e *Engine) saveSessionState() error {
	state, err := config.LoadState(e.Root)
	if err != nil {
		return err
	}
	state.Sessions = e.Sessions
	return config.SaveState(e.Root, state)
}

func (e *Engine) checkpointPath(taskKey string) string {
	return filepath.Join(e.Root, config.Dir, "state", "checkpoints", taskKey+".json")
}

func (e *Engine) saveCheckpoint(cp taskCheckpoint) error {
	if err := os.MkdirAll(filepath.Dir(e.checkpointPath(fmt.Sprintf("task-%03d", cp.TaskIndex))), 0755); err != nil {
		return err
	}
	cp.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(e.checkpointPath(fmt.Sprintf("task-%03d", cp.TaskIndex)), b, 0600)
}

func (e *Engine) readCheckpoint(taskKey string) string {
	b, err := os.ReadFile(e.checkpointPath(taskKey))
	if err != nil {
		return "No durable checkpoint for this task."
	}
	return limitText(string(b), 5000)
}

func isSessionResumeFailure(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	failure := []string{"not found", "unknown", "invalid", "expired", "failed", "unsupported", "unavailable", "does not exist"}
	for _, marker := range []string{"session", "resume", "thread", "conversation"} {
		if !strings.Contains(lower, marker) {
			continue
		}
		for _, reason := range failure {
			if strings.Contains(lower, reason) {
				return true
			}
		}
	}
	return strings.Contains(lower, "could not resume") || strings.Contains(lower, "cannot resume")
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
	for _, rawLine := range strings.Split(value, "\n") {
		line := strings.ToLower(strings.TrimSpace(rawLine))
		if line == "" {
			continue
		}
		// Agent output often contains valid prose mentioning capacity, limits,
		// or restrictions. Only classify explicit failure-shaped lines as
		// retryable; otherwise a successful report can be retried incorrectly.
		if strings.Contains(line, "429") ||
			strings.Contains(line, "rate limit exceeded") ||
			strings.Contains(line, "rate_limit_exceeded") ||
			strings.Contains(line, "too many requests") ||
			strings.Contains(line, "temporarily unavailable") ||
			strings.Contains(line, "connection reset") ||
			strings.Contains(line, "resource exhausted") ||
			strings.Contains(line, "usage limit reached") ||
			strings.Contains(line, "context window exceeded") {
			return true
		}
		failureSignal := strings.Contains(line, "error") ||
			strings.Contains(line, "failed") ||
			strings.Contains(line, "failure") ||
			strings.Contains(line, "retry") ||
			strings.Contains(line, "exceed")
		if !failureSignal {
			continue
		}
		for _, marker := range []string{"quota", "capacity", "timeout", "timed out", "rate limit", "usage limit"} {
			if strings.Contains(line, marker) {
				return true
			}
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
		fmt.Fprintf(&b, "- %s: %s, %d input chars, %d output chars, context=%s, resumed=%t, %s\n", name, metric.Status, metric.InputChars, metric.OutputChars, metric.ContextMode, metric.SessionResumed, metric.Duration.Round(time.Millisecond))
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

// auditError keeps the durable event ledger concise while preserving the
// actionable tail of an agent or tool failure. Full transcripts remain in the
// encrypted attempt log referenced by the event.
func auditError(err error) string {
	if err == nil {
		return ""
	}
	return summarizeAgentError(errorText(err))
}

func statusFromError(err error) string {
	if err != nil {
		return "failed"
	}
	return "completed"
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
	r := runTestDetailed(ctx, root, command)
	return r.Output, r.Err
}

type testResult struct {
	Output     string
	Err        error
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int
}

func runTestDetailed(ctx context.Context, root, command string) testResult {
	started := time.Now().UTC()
	c := exec.CommandContext(ctx, "sh", "-c", command)
	c.Dir = root
	b, err := c.CombinedOutput()
	result := testResult{Output: string(b), Err: err, StartedAt: started, FinishedAt: time.Now().UTC(), ExitCode: 0}
	if exitErr, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
	}
	return result
}

// parseDecision accepts only one exact protocol marker outside quoted or
// fenced examples. Narrative, bullets, contradictory markers, and adapter
// prose are deliberately rejected because they cannot safely drive state.
func parseDecision(output string, markers ...string) (string, error) {
	allowed := make(map[string]bool, len(markers))
	for _, marker := range markers {
		allowed[marker] = true
	}
	var found []string
	fenced := false
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced || strings.HasPrefix(line, ">") || strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "+ ") {
			continue
		}
		line = strings.TrimSpace(strings.Trim(line, "*_`# "))
		if allowed[line] {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("agent decision protocol invalid: expected exactly one marker (%s), found %d", strings.Join(markers, ", "), len(found))
	}
	return found[0], nil
}

func parseReviewerDecision(output string) (string, error) {
	return parseDecision(output, "STATUS: LGTM", "STATUS: CHANGES_REQUESTED")
}

func parseJudgeDecision(output string) (string, error) {
	return parseDecision(output, "VERDICT: OVERRIDE_APPROVE", "VERDICT: FINAL_INSTRUCTIONS")
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
