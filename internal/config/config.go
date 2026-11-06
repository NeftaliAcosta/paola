package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const Dir = ".paola"

type AgentCommand struct {
	Coder       string `yaml:"coder"`
	Reviewer    string `yaml:"reviewer"`
	Judge       string `yaml:"judge"`
	SessionArgs string `yaml:"session_args,omitempty"`
}

type RoleBudget struct {
	MaxTurns        int    `yaml:"max_turns,omitempty"`
	Effort          string `yaml:"effort,omitempty"`
	MaxInputChars   int    `yaml:"max_input_chars,omitempty"`
	MaxOutputChars  int    `yaml:"max_output_chars,omitempty"`
	MaxInputTokens  int    `yaml:"max_input_tokens,omitempty"`
	MaxOutputTokens int    `yaml:"max_output_tokens,omitempty"`
}

type Config struct {
	ProjectName             string                  `yaml:"project_name"`
	TaskTitle               string                  `yaml:"task_title"`
	TestCommand             string                  `yaml:"test_command"`
	MaxRetries              int                     `yaml:"max_retries"`
	AgentIdleTimeoutMinutes int                     `yaml:"agent_idle_timeout_minutes,omitempty"`
	Simulation              bool                    `yaml:"simulation,omitempty"`
	ReuseSessions           bool                    `yaml:"reuse_sessions,omitempty"`
	WorktreePerRun          bool                    `yaml:"worktree_per_run,omitempty"`
	RetryBackoffSeconds     int                     `yaml:"retry_backoff_seconds,omitempty"`
	Budgets                 map[string]RoleBudget   `yaml:"budgets,omitempty"`
	Agents                  map[string]string       `yaml:"agents"`
	Models                  map[string]string       `yaml:"models"`
	Commands                map[string]AgentCommand `yaml:"commands"`
}
type State struct {
	CurrentTask int               `json:"current_task"`
	Attempt     int               `json:"attempt"`
	Status      string            `json:"status"`
	History     []string          `json:"history"`
	Sessions    map[string]string `json:"sessions,omitempty"`
}

func Default(project string) Config {
	return Config{ProjectName: project, TaskTitle: "", MaxRetries: 1, AgentIdleTimeoutMinutes: 30, Budgets: defaultBudgets(), Agents: map[string]string{"coder": "codex", "reviewer": "codex", "judge": "agy"}, Models: map[string]string{"coder": "", "reviewer": "", "judge": ""}, Commands: map[string]AgentCommand{
		"agy":    {Coder: `agy --dangerously-skip-permissions {{model_args}} -p {{prompt}}`, Reviewer: `agy --dangerously-skip-permissions {{model_args}} -p {{prompt}}`, Judge: `agy --dangerously-skip-permissions {{model_args}} -p {{prompt}}`},
		"codex":  {Coder: `codex exec --full-auto {{model_args}} {{effort_args}} {{prompt}}`, Reviewer: `codex exec {{model_args}} {{effort_args}} {{prompt}}`, Judge: `codex exec {{model_args}} {{effort_args}} {{prompt}}`},
		"claude": {Coder: `claude --dangerously-skip-permissions {{model_args}} {{effort_args}} {{session_args}} -p {{prompt}}`, Reviewer: `claude --dangerously-skip-permissions {{model_args}} {{effort_args}} {{session_args}} -p {{prompt}}`, Judge: `claude --dangerously-skip-permissions {{model_args}} {{effort_args}} {{session_args}} -p {{prompt}}`, SessionArgs: `--resume {{session_id}}`},
	}}
}

func defaultBudgets() map[string]RoleBudget {
	return map[string]RoleBudget{
		"coder":    {MaxTurns: 2, Effort: "medium", MaxInputChars: 24000, MaxOutputChars: 60000, MaxInputTokens: 6000, MaxOutputTokens: 15000},
		"reviewer": {MaxTurns: 2, Effort: "low", MaxInputChars: 50000, MaxOutputChars: 30000, MaxInputTokens: 12500, MaxOutputTokens: 7500},
		"judge":    {MaxTurns: 1, Effort: "low", MaxInputChars: 50000, MaxOutputChars: 20000, MaxInputTokens: 12500, MaxOutputTokens: 5000},
	}
}

func (c Config) Budget(role string) RoleBudget {
	defaults := defaultBudgets()[role]
	budget, ok := c.Budgets[role]
	if !ok {
		return defaults
	}
	if budget.MaxTurns <= 0 {
		budget.MaxTurns = defaults.MaxTurns
	}
	if budget.Effort == "" {
		budget.Effort = defaults.Effort
	}
	if budget.MaxInputChars <= 0 {
		budget.MaxInputChars = defaults.MaxInputChars
	}
	if budget.MaxOutputChars <= 0 {
		budget.MaxOutputChars = defaults.MaxOutputChars
	}
	if budget.MaxInputTokens <= 0 {
		budget.MaxInputTokens = defaults.MaxInputTokens
	}
	if budget.MaxOutputTokens <= 0 {
		budget.MaxOutputTokens = defaults.MaxOutputTokens
	}
	return budget
}

func (c Config) EffectiveMaxRetries() int {
	if c.MaxRetries > 0 {
		return c.MaxRetries
	}
	return 1
}

func (c Config) EffectiveIdleTimeoutMinutes() int {
	if c.AgentIdleTimeoutMinutes > 0 {
		return c.AgentIdleTimeoutMinutes
	}
	return 30
}

func (c Config) EffectiveRetryBackoffSeconds() int {
	if c.RetryBackoffSeconds > 0 {
		return c.RetryBackoffSeconds
	}
	return 3
}

func Load(root string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(root, Dir, "config.yaml"))
	if err != nil {
		return c, err
	}
	err = yaml.Unmarshal(b, &c)
	return c, err
}
func Save(root string, c Config) error {
	ApplyAgyPermissions(&c)
	ApplyModelTemplates(&c)
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, Dir, "config.yaml"), b, 0644)
}

// ApplyAgyPermissions migrates the original agy templates while preserving
// unrelated custom command templates. This keeps existing workspaces safe for
// headless runs without requiring users to edit .paola/config.yaml manually.
func ApplyAgyPermissions(c *Config) {
	if c == nil || c.Commands == nil {
		return
	}
	agy := c.Commands["agy"]
	agy.Coder = safeAgyCommand(agy.Coder)
	agy.Reviewer = safeAgyCommand(agy.Reviewer)
	agy.Judge = safeAgyCommand(agy.Judge)
	c.Commands["agy"] = agy
	claude := c.Commands["claude"]
	if !strings.Contains(claude.Coder, "{{model_args}}") {
		claude.Coder = strings.Replace(claude.Coder, "{{prompt}}", "{{model_args}} {{prompt}}", 1)
	}
	if !strings.Contains(claude.Reviewer, "{{model_args}}") {
		claude.Reviewer = strings.Replace(claude.Reviewer, "{{prompt}}", "{{model_args}} {{prompt}}", 1)
	}
	if !strings.Contains(claude.Judge, "{{model_args}}") {
		claude.Judge = strings.Replace(claude.Judge, "{{prompt}}", "{{model_args}} {{prompt}}", 1)
	}
	c.Commands["claude"] = claude
}

func safeAgyCommand(command string) string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "agy -p {{prompt}}" {
		return "agy --dangerously-skip-permissions -p {{prompt}}"
	}
	return command
}

func ApplyModelTemplates(c *Config) {
	if c == nil || c.Commands == nil {
		return
	}
	codex := c.Commands["codex"]
	if !strings.Contains(codex.Coder, "{{model_args}}") {
		codex.Coder = strings.Replace(codex.Coder, "{{prompt}}", "{{model_args}} {{prompt}}", 1)
	}
	if !strings.Contains(codex.Reviewer, "{{model_args}}") {
		codex.Reviewer = strings.Replace(codex.Reviewer, "{{prompt}}", "{{model_args}} {{prompt}}", 1)
	}
	if !strings.Contains(codex.Judge, "{{model_args}}") {
		codex.Judge = strings.Replace(codex.Judge, "{{prompt}}", "{{model_args}} {{prompt}}", 1)
	}
	c.Commands["codex"] = codex
	agy := c.Commands["agy"]
	if !strings.Contains(agy.Coder, "{{model_args}}") {
		agy.Coder = strings.Replace(agy.Coder, "-p {{prompt}}", "{{model_args}} -p {{prompt}}", 1)
	}
	if !strings.Contains(agy.Reviewer, "{{model_args}}") {
		agy.Reviewer = strings.Replace(agy.Reviewer, "-p {{prompt}}", "{{model_args}} -p {{prompt}}", 1)
	}
	if !strings.Contains(agy.Judge, "{{model_args}}") {
		agy.Judge = strings.Replace(agy.Judge, "-p {{prompt}}", "{{model_args}} -p {{prompt}}", 1)
	}
	c.Commands["agy"] = agy
}
func SaveState(root string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, Dir, "state", "state.json"), append(b, '\n'), 0644)
}
func LoadState(root string) (State, error) {
	var s State
	b, err := os.ReadFile(filepath.Join(root, Dir, "state", "state.json"))
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}
func Validate(root string) []string {
	required := []string{"config.yaml", "CONTEXT.md", "ROADMAP.md", "GOLDEN_RULES.md", "roles/coder.md", "roles/reviewer.md", "roles/judge.md", "state/state.json", "state/current.diff", "state/feedback.md", "state/test_output.log"}
	var problems []string
	for _, p := range required {
		info, err := os.Stat(filepath.Join(root, Dir, p))
		allowEmpty := p == "state/current.diff" || p == "state/test_output.log"
		if err != nil || (!allowEmpty && info.Size() == 0) {
			problems = append(problems, p)
		}
	}
	return problems
}
func BinaryStatus(c Config) map[string]string {
	result := map[string]string{}
	for _, name := range []string{"agy", "codex", "claude"} {
		if p, err := exec.LookPath(name); err == nil {
			result[name] = p
		} else {
			result[name] = "not found"
		}
	}
	return result
}
func Check(root string) error {
	if _, err := Load(root); err != nil {
		return err
	}
	if p := Validate(root); len(p) > 0 {
		return fmt.Errorf("missing or empty workspace files: %s", strings.Join(p, ", "))
	}
	return nil
}
func EnsureDir(root string) error {
	if root == "" {
		return errors.New("empty project root")
	}
	return os.MkdirAll(filepath.Join(root, Dir), 0755)
}
