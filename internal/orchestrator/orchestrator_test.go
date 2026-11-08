package orchestrator

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/neftaliacosta/paola/internal/config"
	"github.com/neftaliacosta/paola/internal/history"
)

func TestExtractSessionID(t *testing.T) {
	got := extractSessionID(`session_id: "0123456789abcdef-1234"`)
	if got != "0123456789abcdef-1234" {
		t.Fatalf("session id = %q", got)
	}
}

func TestParseTokenMetrics(t *testing.T) {
	in, out := parseTokenMetrics("input tokens: 1,200\noutput tokens used 345\n")
	if in != 1200 || out != 345 {
		t.Fatalf("token metrics = %d/%d, want 1200/345", in, out)
	}
}

func TestTaskTokenCountPrefersAgentMetrics(t *testing.T) {
	got := taskTokenCount(t.TempDir(), map[string]history.AgentMetric{
		"coder":    {InputTokens: 100, OutputTokens: 40, EstimatedTokens: 999},
		"reviewer": {EstimatedTokens: 20},
	})
	if got != 160 {
		t.Fatalf("task token count = %d, want 160", got)
	}
}

func TestRetryableTextDetectsQuotaOutput(t *testing.T) {
	if !retryableText("Error: resource exhausted; retry after reset") {
		t.Fatal("quota output was not classified as retryable")
	}
	if retryableText("all tests passed") {
		t.Fatal("normal output was classified as retryable")
	}
}

func TestRetryableTextIgnoresSuccessfulReportMentioningCapacity(t *testing.T) {
	output := "Checksums verified successfully.\nDocumented limitation: shared orchestration state changed externally; tracked isolation remains verified."
	if retryableText(output) {
		t.Fatal("successful report was classified as retryable")
	}
}

func TestDecisionProtocolIsStrict(t *testing.T) {
	if got, err := parseJudgeDecision("Evidence\nVERDICT: OVERRIDE_APPROVE"); err != nil || got != "VERDICT: OVERRIDE_APPROVE" {
		t.Fatalf("canonical judge decision = %q, %v", got, err)
	}
	if got, err := parseJudgeDecision("**VERDICT: FINAL_INSTRUCTIONS**"); err != nil || got != "VERDICT: FINAL_INSTRUCTIONS" {
		t.Fatalf("decorated judge decision = %q, %v", got, err)
	}
	for _, input := range []string{"APPROVE", "Judge verdict: APPROVED", "> VERDICT: OVERRIDE_APPROVE", "```\nVERDICT: OVERRIDE_APPROVE\n```", "VERDICT: OVERRIDE_APPROVE\nVERDICT: FINAL_INSTRUCTIONS"} {
		if _, err := parseJudgeDecision(input); err == nil {
			t.Fatalf("accepted unsafe judge output %q", input)
		}
	}
	if got, err := parseReviewerDecision("STATUS: LGTM"); err != nil || got != "STATUS: LGTM" {
		t.Fatalf("reviewer decision = %q, %v", got, err)
	}
}

func TestRoleProtocolRemainsAtPromptTail(t *testing.T) {
	if !strings.HasSuffix(roleProtocol("judge"), "Do not replace it with APPROVE, PASS, PILOT_BLOCKED, or prose.\n") {
		t.Fatal("judge protocol must end with an explicit machine-readable contract")
	}
	if roleProtocol("coder") != "" {
		t.Fatal("coder should not receive a decision protocol")
	}
}

func TestLimitOutputPreservesTail(t *testing.T) {
	input := strings.Repeat("x", 200) + "TAIL"
	got := limitOutput(input, 100)
	if len(got) > 100 {
		t.Fatalf("output length = %d, want <= 100", len(got))
	}
	if !strings.Contains(got, "TAIL") {
		t.Fatal("output lost tail content")
	}
}

func TestLimitTextPreservesUTF8(t *testing.T) {
	input := strings.Repeat("á", 1000) + " ✅ final"
	got := limitText(input, 100)
	if !utf8.ValidString(got) {
		t.Fatal("truncated context is not valid UTF-8")
	}
	if len(got) > 100 {
		t.Fatalf("context length = %d, want <= 100", len(got))
	}
}

func TestLimitTextRepairsInvalidUTF8(t *testing.T) {
	got := limitText(string([]byte("before\x9c\x93 after")), 100)
	if !utf8.ValidString(got) {
		t.Fatal("context repair did not produce valid UTF-8")
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Fatalf("context repair lost surrounding text: %q", got)
	}
}

func TestSessionKeysAreScopedByExecutionTaskAndRole(t *testing.T) {
	e := Engine{ExecutionID: "exec-1"}
	if e.sessionKey("task-001", "coder") == e.sessionKey("task-002", "coder") {
		t.Fatal("different tasks share a session key")
	}
	if e.sessionKey("task-001", "coder") == e.sessionKey("task-001", "reviewer") {
		t.Fatal("different roles share a session key")
	}
}

func TestSessionResumeFailureRequiresFailureEvidence(t *testing.T) {
	if !isSessionResumeFailure(errors.New("session expired; resume unavailable")) {
		t.Fatal("expired session was not detected")
	}
	if isSessionResumeFailure(errors.New("reviewer discussed the session thread")) {
		t.Fatal("ordinary output was classified as session failure")
	}
}

func TestCheckpointRoundTripAndContinuationDetection(t *testing.T) {
	root := t.TempDir()
	e := Engine{Root: root}
	if err := e.saveCheckpoint(taskCheckpoint{Task: "demo", TaskIndex: 1, Attempt: 1, Stage: "reviewer", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	raw := e.readCheckpoint("task-001")
	if !checkpointIsContinuation(raw) {
		t.Fatal("reviewer checkpoint was not marked as continuation")
	}
	info, err := os.Stat(filepath.Join(root, ".paola", "state", "checkpoints", "task-001.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("checkpoint mode = %v; want 0600", info.Mode().Perm())
	}
}

func TestPromptKeepsFirstCoderContextAndCompactsLaterStages(t *testing.T) {
	root := t.TempDir()
	for name, value := range map[string]string{
		"CONTEXT.md":      strings.Repeat("project context ", 20),
		"GOLDEN_RULES.md": strings.Repeat("rule ", 20),
	} {
		path := filepath.Join(root, ".paola", name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	e := Engine{Root: root, ExecutionID: "exec-1", Config: config.Default("demo")}
	if err := e.saveCheckpoint(taskCheckpoint{Task: "demo", TaskIndex: 1, Attempt: 1, Stage: "coder", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	first, err := e.prompt("coder", "demo", "task-001")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "project context") {
		t.Fatal("first coder prompt lost project context")
	}
	if err := e.saveCheckpoint(taskCheckpoint{Task: "demo", TaskIndex: 1, Attempt: 1, Stage: "reviewer", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	later, err := e.prompt("reviewer", "demo", "task-001")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(later, "project context project context project context") {
		t.Fatal("later prompt replayed full static context")
	}
}
