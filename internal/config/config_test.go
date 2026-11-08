package config

import (
	"strings"
	"testing"
)

func TestDefaultCodexCoderCommandUsesSupportedApprovalFlag(t *testing.T) {
	command, ok := DefaultAgentCommand("codex")
	if !ok {
		t.Fatal("codex default command not found")
	}
	if strings.Contains(command.Coder, "--full-auto") {
		t.Fatalf("codex command still contains deprecated --full-auto: %q", command.Coder)
	}
	if !strings.Contains(command.Coder, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("codex command lacks supported approval flag: %q", command.Coder)
	}
	if command.SessionArgs != "resume {{session_id}}" {
		t.Fatalf("codex session args = %q, want durable resume template", command.SessionArgs)
	}
}

func TestDefaultEnablesTaskScopedSessions(t *testing.T) {
	c := Default("demo")
	if !c.SessionsEnabled() || c.EffectiveSessionStrategy() != "task" {
		t.Fatalf("default session strategy = %q, enabled=%v", c.EffectiveSessionStrategy(), c.SessionsEnabled())
	}
}

func TestLegacyConfigGetsTaskSessionsUnlessExplicitlyDisabled(t *testing.T) {
	if !(Config{}).SessionsEnabled() {
		t.Fatal("legacy config should gain task-scoped sessions")
	}
	if (Config{SessionStrategy: "none"}).SessionsEnabled() {
		t.Fatal("explicit stateless strategy should disable sessions")
	}
}

func TestEffectiveDefaultsDoNotMutateLoadedValues(t *testing.T) {
	var c Config
	if got := c.EffectiveMaxRetries(); got != 1 {
		t.Fatalf("effective retries = %d, want 1", got)
	}
	if got := c.EffectiveIdleTimeoutMinutes(); got != 30 {
		t.Fatalf("effective idle timeout = %d, want 30", got)
	}
	if c.MaxRetries != 0 || c.AgentIdleTimeoutMinutes != 0 {
		t.Fatal("effective defaults must not mutate config")
	}
}

func TestBudgetFallback(t *testing.T) {
	c := Config{}
	got := c.Budget("coder")
	if got.MaxTurns != 2 || got.Effort != "medium" || got.MaxInputChars != 24000 || got.MaxInputTokens != 6000 {
		t.Fatalf("unexpected coder fallback: %+v", got)
	}
}

func TestPartialBudgetPreservesConfiguredValuesAndFillsMissingDefaults(t *testing.T) {
	c := Config{Budgets: map[string]RoleBudget{"coder": {MaxTurns: 1, MaxInputTokens: 1000}}}
	got := c.Budget("coder")
	if got.MaxTurns != 1 || got.MaxInputTokens != 1000 || got.MaxOutputTokens != 15000 || got.Effort != "medium" {
		t.Fatalf("unexpected merged budget: %+v", got)
	}
}
