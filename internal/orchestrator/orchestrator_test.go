package orchestrator

import (
	"strings"
	"testing"
	"unicode/utf8"

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
