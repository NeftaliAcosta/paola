package wizard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/huh"
	"github.com/neftaliacosta/paola/internal/config"
)

const defaultCoder = `You are the Coder. Implement the assigned task completely in the current project.

Constraints:
- Inspect existing code before changing it and keep the change focused.
- Do not weaken security, tests, or established project conventions.
- Run the configured deterministic test/build command when possible.
- Leave the working tree with a maintainable implementation and useful comments only where needed.`

const defaultReviewer = `You are the Reviewer. Review the task implementation, current diff, tests, and golden rules.

Your response MUST contain a line exactly equal to STATUS: LGTM or STATUS: CHANGES_REQUESTED.
For CHANGES_REQUESTED, list concrete, actionable defects. Do not request stylistic changes without a rule or correctness reason.`

const defaultJudge = `You are the Judge and pragmatic Principal Architect. Resolve a disagreement after the retry limit.

Your response MUST contain a line exactly equal to VERDICT: OVERRIDE_APPROVE or VERDICT: FINAL_INSTRUCTIONS.
Use OVERRIDE_APPROVE when the reviewer is overly pedantic and the implementation satisfies the rules. Use FINAL_INSTRUCTIONS for a real blocker and give precise instructions for one final coder pass.`

type FileOptions struct {
	Context   string
	Objective string
	Roadmap   string
	Rules     string
}

const navigationHelp = "Controls: ctrl+j/ctrl+enter = new line · enter/tab = next · shift+tab = back · ctrl+c = cancel"

func Run(root string) error { return RunWithFiles(root, FileOptions{}) }

func RunWithFiles(root string, files FileOptions) error {
	project := filepath.Base(root)
	c := config.Default(project)
	existing := workspaceExists(root)
	if existing {
		update := false
		confirm := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().Title("A .paola workspace already exists.").Description("Do you want to reconfigure it? Existing state, history, diffs, and test logs will be preserved.").Affirmative("Yes, reconfigure").Negative("No, keep it").Value(&update),
			).Title("Existing workspace").Description(navigationHelp),
		).WithKeyMap(wizardKeyMap()).WithShowHelp(true)
		if err := confirm.Run(); err != nil {
			return err
		}
		if !update {
			fmt.Println("Existing .paola workspace kept unchanged.")
			return nil
		}
		loaded, err := config.Load(root)
		if err != nil {
			return fmt.Errorf("load existing .paola configuration: %w", err)
		}
		c = loaded
		defaults := config.Default(project)
		if c.ProjectName == "" {
			c.ProjectName = defaults.ProjectName
		}
		if c.Agents == nil {
			c.Agents = defaults.Agents
		}
		if c.Models == nil {
			c.Models = defaults.Models
		}
		if c.Commands == nil {
			c.Commands = defaults.Commands
		}
		for name, command := range defaults.Commands {
			if _, ok := c.Commands[name]; !ok {
				c.Commands[name] = command
			}
		}
	}
	detectAgents()

	contextText, err := readFileOption(files.Context, "context")
	if err != nil {
		return err
	}
	objective, err := readFileOption(files.Objective, "objective")
	if err != nil {
		return err
	}
	roadmap, err := readFileOption(files.Roadmap, "roadmap")
	if err != nil {
		return err
	}
	rules, err := readFileOption(files.Rules, "rules")
	if err != nil {
		return err
	}
	test := ""
	retries := "1"
	taskTitle := ""
	coderPrompt, reviewerPrompt, judgePrompt := defaultCoder, defaultReviewer, defaultJudge
	coder, reviewer, judge := "codex", "codex", "agy"
	modelCoder, modelReviewer, modelJudge := "", "", ""
	effortCoder, effortReviewer, effortJudge := "medium", "low", "low"
	savedContext, savedObjective, savedRoadmap, savedRules := "", "", "", ""
	savedTest, savedRetries, savedTitle := "", "", ""
	savedCoderPrompt, savedReviewerPrompt, savedJudgePrompt := "", "", ""
	savedModelCoder, savedModelReviewer, savedModelJudge := "", "", ""
	customizeRoles := false
	if existing {
		storedContext, storedObjective := loadContextDefaults(root)
		savedContext, savedObjective = storedContext, storedObjective
		savedRoadmap = readWorkspaceFile(root, "ROADMAP.md")
		savedRules = readWorkspaceFile(root, "GOLDEN_RULES.md")
		if contextText == "" {
			contextText = ""
		}
		if objective == "" {
			objective = ""
		}
		if roadmap == "" {
			roadmap = ""
		}
		if rules == "" {
			rules = ""
		}
		if value := readWorkspaceFile(root, "roles/coder.md"); value != "" {
			savedCoderPrompt = value
		}
		if value := readWorkspaceFile(root, "roles/reviewer.md"); value != "" {
			savedReviewerPrompt = value
		}
		if value := readWorkspaceFile(root, "roles/judge.md"); value != "" {
			savedJudgePrompt = value
		}
		savedTest = c.TestCommand
		savedRetries = strconv.Itoa(c.MaxRetries)
		if c.MaxRetries > 0 {
			retries = ""
		}
		if value := c.Agents["coder"]; value != "" {
			coder = value
		}
		if value := c.Agents["reviewer"]; value != "" {
			reviewer = value
		}
		if value := c.Agents["judge"]; value != "" {
			judge = value
		}
		savedModelCoder, savedModelReviewer, savedModelJudge = c.Models["coder"], c.Models["reviewer"], c.Models["judge"]
		effortCoder, effortReviewer, effortJudge = c.Budget("coder").Effort, c.Budget("reviewer").Effort, c.Budget("judge").Effort
		modelCoder, modelReviewer, modelJudge = "", "", ""
		savedTitle = c.TaskTitle
		taskTitle = ""
	}
	if effortCoder == "" {
		effortCoder = "medium"
	}
	if effortReviewer == "" {
		effortReviewer = "low"
	}
	if effortJudge == "" {
		effortJudge = "low"
	}
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title("What should this task be called?").Description("Use a short, recognizable title for this objective.").Value(&taskTitle),
		).Title("1/7 · Task title").Description(step("This title groups all future iterations and reports for the same objective.")),
		huh.NewGroup(
			huh.NewText().Title("What are we working on?").Description("Describe the project, architecture, and constraints. Ctrl+E opens $EDITOR.").CharLimit(0).Value(&contextText),
		).Title("2/7 · Project context").Description(step("This helps the agents understand the codebase before making changes.")),
		huh.NewGroup(
			huh.NewText().Title("What should paola deliver?").Description("Describe the final outcome in plain language. Ctrl+E opens $EDITOR.").CharLimit(0).Value(&objective),
		).Title("3/7 · Final objective").Description(step("State the result that means this project is done.")),
		huh.NewGroup(
			huh.NewText().Title("What needs to happen, step by step?").Description("One task per line. Checklist prefixes are optional. Ctrl+E opens $EDITOR.").CharLimit(0).Value(&roadmap),
		).Title("4/7 · Roadmap").Description(step("Keep tasks concrete and ordered; paola will execute unchecked tasks in sequence.")),
		huh.NewGroup(
			huh.NewConfirm().Title("Customize agent instructions?").Description("Recommended defaults are ready. Choose Yes only if you need special role constraints.").Affirmative("Yes, customize").Negative("No, use defaults").Value(&customizeRoles),
		).Title("5/7 · Agent instructions").Description(step("Most projects can safely use the recommended Coder, Reviewer, and Judge prompts.")),
		huh.NewGroup(
			huh.NewSelect[string]().Title("Who writes code?").Options(huh.NewOptions("agy", "codex", "claude")...).Value(&coder),
			huh.NewSelect[string]().Title("Who reviews changes?").Options(huh.NewOptions("agy", "codex", "claude")...).Value(&reviewer),
			huh.NewSelect[string]().Title("Who breaks ties?").Options(huh.NewOptions("agy", "codex", "claude")...).Value(&judge),
			huh.NewInput().Title("Coder model (optional)").Description("Example: sonnet, o3, gpt-5").Value(&modelCoder),
			huh.NewInput().Title("Reviewer model (optional)").Value(&modelReviewer),
			huh.NewInput().Title("Judge model (optional)").Value(&modelJudge),
			huh.NewSelect[string]().Title("Coder effort").Options(huh.NewOptions("low", "medium", "high")...).Value(&effortCoder),
			huh.NewSelect[string]().Title("Reviewer effort").Options(huh.NewOptions("low", "medium", "high")...).Value(&effortReviewer),
			huh.NewSelect[string]().Title("Judge effort").Options(huh.NewOptions("low", "medium", "high")...).Value(&effortJudge),
		).Title("6/7 · Assign local AI tools").Description(step("You can use the same CLI for multiple roles.")),
		huh.NewGroup(
			huh.NewText().Title("What rules must always be followed?").Description("Coding standards, forbidden patterns, security rules, and Definition of Done. Ctrl+E opens $EDITOR.").CharLimit(0).Value(&rules),
			huh.NewInput().Title("Test/build command (optional)").Description("Example: go test ./... or npm test").Value(&test),
			huh.NewInput().Title("Retries before Judge").Description("Default: 1").Value(&retries),
		).Title("7/7 · Rules and validation").Description(step("The test command runs locally before an agent review.")),
	).WithKeyMap(wizardKeyMap()).WithShowHelp(true)
	if err := form.Run(); err != nil {
		return err
	}
	if existing {
		if strings.TrimSpace(contextText) == "" {
			contextText = savedContext
		}
		if strings.TrimSpace(objective) == "" {
			objective = savedObjective
		}
		if strings.TrimSpace(roadmap) == "" {
			roadmap = savedRoadmap
		}
		if strings.TrimSpace(rules) == "" {
			rules = savedRules
		}
		if strings.TrimSpace(test) == "" {
			test = savedTest
		}
		if strings.TrimSpace(retries) == "" {
			retries = savedRetries
		}
		if strings.TrimSpace(taskTitle) == "" {
			taskTitle = savedTitle
		}
		if strings.TrimSpace(modelCoder) == "" {
			modelCoder = savedModelCoder
		}
		if strings.TrimSpace(modelReviewer) == "" {
			modelReviewer = savedModelReviewer
		}
		if strings.TrimSpace(modelJudge) == "" {
			modelJudge = savedModelJudge
		}
	}
	if existing && customizeRoles {
		coderPrompt, reviewerPrompt, judgePrompt = "", "", ""
	}
	if customizeRoles {
		roleForm := huh.NewForm(
			huh.NewGroup(
				huh.NewText().Title("Coder instructions").Description("Tell the Coder how to implement tasks. Ctrl+E opens $EDITOR.").CharLimit(0).Value(&coderPrompt),
			).Title("5a/7 · Coder instructions").Description(step("The default already includes safe implementation and testing constraints.")),
			huh.NewGroup(
				huh.NewText().Title("Reviewer instructions").Description("The Reviewer must emit STATUS: LGTM or STATUS: CHANGES_REQUESTED. Ctrl+E opens $EDITOR.").CharLimit(0).Value(&reviewerPrompt),
			).Title("5b/7 · Reviewer instructions").Description(step("Keep the required status protocol intact.")),
			huh.NewGroup(
				huh.NewText().Title("Judge instructions").Description("The Judge must emit VERDICT: OVERRIDE_APPROVE or VERDICT: FINAL_INSTRUCTIONS. Ctrl+E opens $EDITOR.").CharLimit(0).Value(&judgePrompt),
			).Title("5c/7 · Judge instructions").Description(step("Keep the required verdict protocol intact.")),
		).WithKeyMap(wizardKeyMap()).WithShowHelp(true)
		if err := roleForm.Run(); err != nil {
			return err
		}
	}
	if existing {
		if customizeRoles {
			if strings.TrimSpace(coderPrompt) == "" {
				coderPrompt = savedCoderPrompt
			}
			if strings.TrimSpace(reviewerPrompt) == "" {
				reviewerPrompt = savedReviewerPrompt
			}
			if strings.TrimSpace(judgePrompt) == "" {
				judgePrompt = savedJudgePrompt
			}
		} else {
			if savedCoderPrompt != "" {
				coderPrompt = savedCoderPrompt
			}
			if savedReviewerPrompt != "" {
				reviewerPrompt = savedReviewerPrompt
			}
			if savedJudgePrompt != "" {
				judgePrompt = savedJudgePrompt
			}
		}
	}
	if strings.TrimSpace(contextText) == "" {
		contextText = "Project context not provided. Inspect the repository before editing."
	}
	if strings.TrimSpace(taskTitle) == "" {
		taskTitle = "Untitled task"
	}
	if strings.TrimSpace(objective) == "" {
		objective = "Complete the requested project objective."
	}
	if strings.TrimSpace(roadmap) == "" {
		roadmap = "Implement the requested project"
	}
	if strings.TrimSpace(rules) == "" {
		rules = "Keep changes focused, tested, secure, and documented."
	}
	if n, err := strconv.Atoi(strings.TrimSpace(retries)); err == nil && n > 0 {
		c.MaxRetries = n
	}
	c.TestCommand = strings.TrimSpace(test)
	c.TaskTitle = strings.TrimSpace(taskTitle)
	c.Agents["coder"], c.Agents["reviewer"], c.Agents["judge"] = coder, reviewer, judge
	if c.Models == nil {
		c.Models = map[string]string{}
	}
	c.Models["coder"], c.Models["reviewer"], c.Models["judge"] = strings.TrimSpace(modelCoder), strings.TrimSpace(modelReviewer), strings.TrimSpace(modelJudge)
	if c.Budgets == nil {
		c.Budgets = config.Default(project).Budgets
	}
	c.Budgets["coder"] = withEffort(c.Budget("coder"), effortCoder)
	c.Budgets["reviewer"] = withEffort(c.Budget("reviewer"), effortReviewer)
	c.Budgets["judge"] = withEffort(c.Budget("judge"), effortJudge)

	if err := config.EnsureDir(root); err != nil {
		return err
	}
	base := filepath.Join(root, config.Dir)
	generated := map[string]string{
		"CONTEXT.md":        "# Project Context\n\n" + strings.TrimSpace(contextText) + "\n\n# Final Objective\n\n" + strings.TrimSpace(objective) + "\n",
		"ROADMAP.md":        checklist(roadmap),
		"GOLDEN_RULES.md":   strings.TrimSpace(rules) + "\n",
		"roles/coder.md":    strings.TrimSpace(coderPrompt) + "\n",
		"roles/reviewer.md": strings.TrimSpace(reviewerPrompt) + "\n",
		"roles/judge.md":    strings.TrimSpace(judgePrompt) + "\n",
	}
	if !existing {
		generated["state/current.diff"] = "No diff captured yet.\n"
		generated["state/feedback.md"] = "No feedback yet.\n"
		generated["state/test_output.log"] = "No test run yet.\n"
	}
	for name, body := range generated {
		p := filepath.Join(base, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			return err
		}
	}
	if existing {
		stateDefaults := map[string]string{"state/current.diff": "No diff captured yet.\n", "state/feedback.md": "No feedback yet.\n", "state/test_output.log": "No test run yet.\n"}
		for name, body := range stateDefaults {
			p := filepath.Join(base, name)
			info, statErr := os.Stat(p)
			if os.IsNotExist(statErr) || (statErr == nil && info.Size() == 0) {
				if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
					return err
				}
				if err := os.WriteFile(p, []byte(body), 0644); err != nil {
					return err
				}
			}
		}
	}
	if err := config.Save(root, c); err != nil {
		return err
	}
	if !existing {
		if err := config.SaveState(root, config.State{Status: "ready", History: []string{"workspace initialized"}}); err != nil {
			return err
		}
	} else if _, err := os.Stat(filepath.Join(base, "state", "state.json")); os.IsNotExist(err) {
		if err := config.SaveState(root, config.State{Status: "ready", History: []string{"workspace reconfigured"}}); err != nil {
			return err
		}
	}
	if problems := config.Validate(root); len(problems) > 0 {
		return fmt.Errorf("validation failed: %v", problems)
	}
	fmt.Println("✓ .paola/ initialized; paola run is ready")
	return nil
}

func withEffort(b config.RoleBudget, effort string) config.RoleBudget {
	b.Effort = effort
	return b
}

func readFileOption(path, label string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s file %q: %w", label, path, err)
	}
	return string(b), nil
}

func workspaceExists(root string) bool {
	info, err := os.Stat(filepath.Join(root, config.Dir))
	return err == nil && info.IsDir()
}

func readWorkspaceFile(root, name string) string {
	b, err := os.ReadFile(filepath.Join(root, config.Dir, name))
	if err != nil {
		return ""
	}
	return string(b)
}

func loadContextDefaults(root string) (string, string) {
	content := readWorkspaceFile(root, "CONTEXT.md")
	if content == "" {
		return "", ""
	}
	return markdownSection(content, "Project Context"), markdownSection(content, "Final Objective")
}

func markdownSection(content, heading string) string {
	marker := "# " + heading
	start := strings.Index(content, marker)
	if start < 0 {
		return ""
	}
	start += len(marker)
	if start < len(content) && content[start] == '\n' {
		start++
	}
	rest := content[start:]
	if next := strings.Index(rest, "\n# "); next >= 0 {
		rest = rest[:next]
	}
	return strings.TrimSpace(rest)
}

func step(description string) string { return description + "\n\n" + navigationHelp }

func wizardKeyMap() *huh.KeyMap {
	keymap := huh.NewDefaultKeyMap()
	keymap.Text.Next = key.NewBinding(
		key.WithKeys("enter", "tab"),
		key.WithHelp("enter/tab", "next"),
	)
	keymap.Text.Submit = key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "next"),
	)
	keymap.Text.NewLine = key.NewBinding(
		key.WithKeys("ctrl+enter", "alt+enter", "ctrl+j"),
		key.WithHelp("ctrl+j/ctrl+enter", "new line"),
	)
	return keymap
}

func detectAgents() {
	fmt.Println("Detected local agent CLIs:")
	for _, name := range []string{"agy", "codex", "claude"} {
		if path, err := exec.LookPath(name); err == nil {
			fmt.Printf("  ✓ %s: %s\n", name, path)
		} else {
			fmt.Printf("  · %s: not found (you can still configure it later)\n", name)
		}
	}
}

func checklist(s string) string {
	var out strings.Builder
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "- [ ]") || strings.HasPrefix(line, "- [x]") || strings.HasPrefix(line, "- [X]") {
			out.WriteString(line)
		} else {
			out.WriteString("- [ ] ")
			out.WriteString(line)
		}
		out.WriteByte('\n')
	}
	return out.String()
}
