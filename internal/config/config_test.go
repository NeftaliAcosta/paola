package config

import "testing"

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
