package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/neftaliacosta/paola/internal/agent"
	"github.com/neftaliacosta/paola/internal/config"
	"github.com/neftaliacosta/paola/internal/gitutil"
	paolahistory "github.com/neftaliacosta/paola/internal/history"
	"github.com/neftaliacosta/paola/internal/orchestrator"
	"github.com/neftaliacosta/paola/internal/tui"
	"github.com/neftaliacosta/paola/internal/wizard"
	"github.com/spf13/cobra"
)

const version = "0.1.10"

var (
	headless      bool
	baseBranch    string
	contextFile   string
	objectiveFile string
	roadmapFile   string
	rulesFile     string
)
var rootCmd = &cobra.Command{Use: "paola", Short: "Polymorphic Autonomous Orchestrator for Local Agents", Version: version, SilenceUsage: true}

func init() {
	rootCmd.SetVersionTemplate("paola version {{.Version}}\n" + runtime.GOOS + "/" + runtime.GOARCH + " · Go " + runtime.Version() + "\n")
	initCmd := &cobra.Command{Use: "init", Short: "Initialize a .paola workspace", RunE: func(*cobra.Command, []string) error {
		root, _ := os.Getwd()
		if err := wizard.RunWithFiles(root, wizard.FileOptions{Context: contextFile, Objective: objectiveFile, Roadmap: roadmapFile, Rules: rulesFile}); err != nil {
			return err
		}
		if gitutil.IsRepo(root) {
			if err := gitutil.EnsureLocalExclude(root); err != nil {
				return fmt.Errorf("update local Git exclusions: %w", err)
			}
		}
		return nil
	}}
	initCmd.Flags().StringVar(&contextFile, "context-file", "", "load project context from a file")
	initCmd.Flags().StringVar(&objectiveFile, "objective-file", "", "load final objective from a file")
	initCmd.Flags().StringVar(&roadmapFile, "roadmap-file", "", "load roadmap tasks from a file")
	initCmd.Flags().StringVar(&rulesFile, "rules-file", "", "load golden rules from a file")
	rootCmd.AddCommand(initCmd)
	run := &cobra.Command{Use: "run", Short: "Run the autonomous roadmap", RunE: runCommand}
	run.Flags().BoolVar(&headless, "headless", false, "disable the live terminal UI")
	run.Flags().StringVar(&baseBranch, "base-branch", "", "base branch for the isolated task branch")
	rootCmd.AddCommand(run)
	rootCmd.AddCommand(&cobra.Command{Use: "config", Short: "Show configuration and agent health", RunE: configCommand})
	rootCmd.AddCommand(&cobra.Command{Use: "history", Short: "List previous orchestration runs", RunE: historyCommand})
	rootCmd.AddCommand(&cobra.Command{Use: "update", Short: "Check self-update status", RunE: func(*cobra.Command, []string) error {
		fmt.Printf("paola %s is current; self-update provider is ready for GitHub releases.\n", version)
		return nil
	}})
}
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
func root() (string, error) { return os.Getwd() }
func configCommand(*cobra.Command, []string) error {
	r, err := root()
	if err != nil {
		return err
	}
	c, err := config.Load(r)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	fmt.Println("PAOLA CONFIG")
	fmt.Println("────────────")
	fmt.Printf("Project: %s\nRetries: %d\nTest: %s\n", c.ProjectName, c.MaxRetries, empty(c.TestCommand))
	fmt.Println("Role      CLI")
	for _, role := range []string{"coder", "reviewer", "judge"} {
		model := c.Models[role]
		if model == "" {
			model = "default"
		}
		fmt.Printf("%-9s %-8s model:%s\n", role, c.Agents[role], model)
	}
	fmt.Println("Binaries")
	for n, p := range config.BinaryStatus(c) {
		fmt.Printf("%-9s %s\n", n, p)
	}
	if err := config.Check(r); err != nil {
		fmt.Println("Doctor: FAIL -", err)
	} else {
		fmt.Println("Doctor: OK")
	}
	return nil
}
func runCommand(*cobra.Command, []string) error {
	r, err := root()
	if err != nil {
		return err
	}
	if !gitutil.IsRepo(r) {
		return fmt.Errorf("current directory is not a Git repository. Initialize git with 'git init' before running paola.")
	}
	c, err := config.Load(r)
	if err != nil {
		return fmt.Errorf(".paola not initialized: %w", err)
	}
	branch, err := prepareRunBranch(r, c, headless, baseBranch)
	if err != nil {
		return err
	}
	if headless {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		e := &orchestrator.Engine{Root: r, Config: c, Runner: agent.Runner{}, Control: orchestrator.NewControl(cancel), TaskBranch: branch.Task, BaseBranch: branch.Base, Log: func(s string) { fmt.Println(s) }}
		return e.Run(ctx)
	}
	for {
		err = runInteractive(r, c, branch)
		action, menuErr := postRunMenu(r, err)
		if menuErr != nil {
			return menuErr
		}
		switch action {
		case "retry":
			continue
		case "new":
			if err := wizard.Run(r); err != nil {
				return err
			}
			c, err = config.Load(r)
			if err != nil {
				return err
			}
			branch, err = prepareRunBranch(r, c, false, "")
			if err != nil {
				return err
			}
			continue
		case "history":
			if err := printHistory(r); err != nil {
				return err
			}
			continue
		case "permissions":
			if err := enableHeadlessPermissions(r); err != nil {
				return err
			}
			c, err = config.Load(r)
			if err != nil {
				return err
			}
			continue
		default:
			return nil
		}
	}
}

func prepareRunBranch(root string, c config.Config, headlessMode bool, requestedBase string) (gitutil.BranchInfo, error) {
	current, err := gitutil.CurrentBranch(root)
	if err != nil {
		return gitutil.BranchInfo{}, err
	}
	branches, err := gitutil.Branches(root)
	if err != nil {
		return gitutil.BranchInfo{}, err
	}
	base := requestedBase
	if base == "" {
		base = current
	}
	if headlessMode && requestedBase == "" {
		base = current
	}
	if !containsString(branches, base) {
		return gitutil.BranchInfo{}, fmt.Errorf("base branch %q does not exist locally", base)
	}
	if !headlessMode {
		options := make([]huh.Option[string], 0, len(branches))
		for _, branch := range branches {
			options = append(options, huh.NewOption(branch, branch))
		}
		form := huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Select base branch to branch off from:").Options(options...).Value(&base)).Title("Git isolation").Description("Paola will create a separate task branch and leave it checked out."))
		if err := form.Run(); err != nil {
			return gitutil.BranchInfo{}, err
		}
	}

	taskBranch := gitutil.TaskBranch(c.TaskTitle)
	if gitutil.HasBranch(root, taskBranch) {
		if !headlessMode {
			choice := "continue"
			form := huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Task branch already exists").Options(
				huh.NewOption("Continue on "+taskBranch, "continue"),
				huh.NewOption("Use an alternate task branch", "alternate"),
			).Value(&choice)).Title("Git isolation").Description("Choose whether to reuse the existing isolated branch."))
			if err := form.Run(); err != nil {
				return gitutil.BranchInfo{}, err
			}
			if choice == "alternate" {
				alternate := ""
				form := huh.NewForm(huh.NewGroup(huh.NewInput().Title("Alternate task branch name").Description("A paola/ prefix will be added automatically.").Value(&alternate)).Title("Git isolation"))
				if err := form.Run(); err != nil {
					return gitutil.BranchInfo{}, err
				}
				taskBranch = gitutil.TaskBranch(alternate)
			}
		} else {
			// Headless runs are deterministic: reuse the existing task branch.
		}
		if gitutil.HasBranch(root, taskBranch) && taskBranch != base {
			if err := gitutil.CheckoutTaskBranch(root, base, taskBranch); err != nil {
				return gitutil.BranchInfo{}, err
			}
			return gitutil.BranchInfo{Base: base, Task: taskBranch}, nil
		}
		if taskBranch == base {
			return gitutil.BranchInfo{Base: base, Task: taskBranch}, nil
		}
		return gitutil.BranchInfo{}, fmt.Errorf("task branch %q already exists; choose another branch name", taskBranch)
	}
	if err := gitutil.CheckoutTaskBranch(root, base, taskBranch); err != nil {
		return gitutil.BranchInfo{}, err
	}
	return gitutil.BranchInfo{Base: base, Task: taskBranch}, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func runInteractive(root string, c config.Config, branch gitutil.BranchInfo) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	control := orchestrator.NewControl(cancel)
	p := tui.New(filepath.Base(root), control)
	result := make(chan error, 1)
	go func() { _, e := p.Run(); result <- e }()
	time.Sleep(50 * time.Millisecond)
	e := &orchestrator.Engine{Root: root, Config: c, Runner: agent.Runner{}, Control: control, TaskBranch: branch.Task, BaseBranch: branch.Base, Log: func(s string) { tui.Log(p, s) }, Update: func(state orchestrator.Progress) { tui.UpdateState(p, state) }}
	err := e.Run(ctx)
	tui.Finish(p)
	if uiErr := <-result; err == nil {
		err = uiErr
	}
	// Bubble Tea restores the alternate screen asynchronously in some terminal
	// emulators. Give it a moment and explicitly restore cursor/style state
	// before huh starts reading keys for the post-run menu.
	time.Sleep(50 * time.Millisecond)
	fmt.Fprint(os.Stdout, "\033[0m\033[?25h")
	return err
}

func postRunMenu(root string, runErr error) (string, error) {
	status := "Execution completed."
	if runErr != nil {
		status = "Execution paused with an error: " + runErr.Error()
	}
	choice := "exit"
	historyLabel := "View history"
	if runErr != nil {
		historyLabel = "View failure details"
	}
	options := []huh.Option[string]{huh.NewOption("Exit paola", "exit"), huh.NewOption(historyLabel, "history"), huh.NewOption("Create or reconfigure a run", "new")}
	if runErr != nil {
		options = append(options, huh.NewOption("Retry pending task", "retry"))
		lower := strings.ToLower(runErr.Error())
		if strings.Contains(lower, "codex") && (strings.Contains(lower, "permission") || strings.Contains(lower, "denied")) {
			options = append(options, huh.NewOption("Enable Codex approval mode and retry", "permissions"))
		}
	}
	form := huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("What next?").Description(status).Options(options...).Value(&choice)).Title("Run finished").Description("Review failure details before retrying; the execution was saved to .paola/history/."))
	return choice, form.Run()
}

func enableHeadlessPermissions(root string) error {
	c, err := config.Load(root)
	if err != nil {
		return err
	}
	confirm := false
	form := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Enable Codex approval mode?").Description("Adds --approve-for-me to Codex commands so headless runs can approve workspace-write tools.").Affirmative("Enable").Negative("Cancel").Value(&confirm)))
	if err := form.Run(); err != nil || !confirm {
		return err
	}
	codex := c.Commands["codex"]
	codex.Coder = addCodexApproval(codex.Coder)
	codex.Reviewer = addCodexApproval(codex.Reviewer)
	codex.Judge = addCodexApproval(codex.Judge)
	c.Commands["codex"] = codex
	return config.Save(root, c)
}

func addCodexApproval(command string) string {
	if strings.Contains(command, "--approve-for-me") {
		return command
	}
	return strings.Replace(command, "codex exec", "codex exec --approve-for-me", 1)
}

func historyCommand(_ *cobra.Command, args []string) error {
	r, err := root()
	if err != nil {
		return err
	}
	if len(args) > 0 {
		run, err := paolahistory.Load(r, args[0])
		if err != nil {
			return err
		}
		return showHistoryRun(r, run)
	}
	return printHistory(r)
}
func printHistory(root string) error {
	runs, err := paolahistory.List(root)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Println("No orchestration runs recorded yet.")
		return nil
	}
	groups := map[string][]paolahistory.Manifest{}
	for _, run := range runs {
		groups[run.TaskTitle] = append(groups[run.TaskTitle], run)
	}
	titles := make([]string, 0, len(groups))
	for title := range groups {
		titles = append(titles, title)
	}
	// Show the task with the most recent execution first. Keep the title as a
	// deterministic tie-breaker when two tasks have the same timestamp.
	sort.Slice(titles, func(i, j int) bool {
		left := latestIteration(groups[titles[i]])
		right := latestIteration(groups[titles[j]])
		if left.StartedAt.Equal(right.StartedAt) {
			return titles[i] < titles[j]
		}
		return left.StartedAt.After(right.StartedAt)
	})
	if len(titles) == 0 {
		return nil
	}
	title := titles[0]
	titleOptions := []huh.Option[string]{huh.NewOption("← Back", "__close_history__")}
	for _, value := range titles {
		titleOptions = append(titleOptions, huh.NewOption(fmt.Sprintf("%s (%d iterations)", value, len(groups[value])), value))
	}
	if err := huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Select a task").Options(titleOptions...).Height(10).Value(&title)).Title("History · Tasks").Description("Newest task first · ↑/↓ or mouse to scroll · Enter to inspect.")).Run(); err != nil {
		return err
	}
	if title == "__close_history__" {
		return nil
	}
	for {
		iterations := groups[title]
		sort.Slice(iterations, func(i, j int) bool { return iterations[i].StartedAt.After(iterations[j].StartedAt) })
		id := iterations[0].ID
		iterationOptions := []huh.Option[string]{huh.NewOption("← Back to tasks", "__back_to_tasks__")}
		for _, run := range iterations {
			end := "running"
			if run.FinishedAt != nil {
				end = run.FinishedAt.In(time.Local).Format("15:04")
			}
			iterationOptions = append(iterationOptions, huh.NewOption(fmt.Sprintf("%s · %s · %s", run.StartedAt.In(time.Local).Format("2006-01-02 15:04"), end, run.Status), run.ID))
		}
		if err := huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Select an iteration").Options(iterationOptions...).Height(10).Value(&id)).Title("History · Iterations").Description("Newest first · ↑/↓ or mouse to scroll · Enter to inspect · Esc to go back")).Run(); err != nil {
			return err
		}
		if id == "__back_to_tasks__" {
			return printHistory(root)
		}
		run, err := paolahistory.Load(root, id)
		if err != nil {
			return err
		}
		if err := showHistoryRun(root, run); err != nil {
			return err
		}
		// Closing the report returns to the iteration selector instead of the shell.
	}
}

func latestIteration(iterations []paolahistory.Manifest) paolahistory.Manifest {
	latest := paolahistory.Manifest{}
	for _, iteration := range iterations {
		if latest.ID == "" || iteration.StartedAt.After(latest.StartedAt) {
			latest = iteration
		}
	}
	return latest
}

type historyFileChange struct {
	label string
	path  string
	diff  string
}

func showHistoryRun(root string, run paolahistory.Manifest) error {
	action, err := tui.ShowReport(run.TaskTitle+" · "+run.ID, renderHistoryReport(run))
	if err != nil {
		return err
	}
	if action != "view_files" {
		return nil
	}
	changes, err := loadHistoryFileChanges(root, run)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return nil
	}

	for {
		choice := "__close__"
		options := []huh.Option[string]{huh.NewOption("← Back to iteration", "__close__")}
		for i, change := range changes {
			options = append(options, huh.NewOption(change.label, fmt.Sprintf("%d", i)))
		}
		form := huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Inspect file changes").Description("Select a file to view added and removed lines.").Options(options...).Value(&choice)).Title("Changed files").Description("The diff is encrypted in the local history and opened in a scrollable viewer."))
		if err := form.Run(); err != nil {
			return err
		}
		if choice == "__close__" {
			return nil
		}
		index, err := strconv.Atoi(choice)
		if err != nil || index < 0 || index >= len(changes) {
			continue
		}
		change := changes[index]
		if _, err := tui.ShowDiffReport("File change · "+change.path, change.diff); err != nil {
			return err
		}
	}
}

func loadHistoryFileChanges(root string, run paolahistory.Manifest) ([]historyFileChange, error) {
	var changes []historyFileChange
	for _, task := range run.Tasks {
		diff, err := paolahistory.LoadDiff(root, run, task.Index)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, chunk := range strings.Split(diff, "diff --git ") {
			chunk = strings.TrimSpace(chunk)
			if chunk == "" {
				continue
			}
			lines := strings.Split(chunk, "\n")
			if len(lines) == 0 {
				continue
			}
			path := strings.TrimSpace(lines[0])
			if fields := strings.Fields(path); len(fields) >= 2 {
				path = strings.TrimPrefix(fields[len(fields)-1], "b/")
			}
			added, removed := 0, 0
			for _, line := range lines[1:] {
				if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
					added++
				}
				if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
					removed++
				}
			}
			content := "diff --git " + chunk
			changes = append(changes, historyFileChange{
				label: fmt.Sprintf("Task %d · %s · +%d -%d", task.Index, path, added, removed),
				path:  path,
				diff:  content,
			})
		}
	}
	return changes, nil
}

func renderHistoryReport(run paolahistory.Manifest) string {
	loc := time.Local
	formatTime := func(value time.Time) string {
		if value.IsZero() {
			return "—"
		}
		return value.In(loc).Format("02 Jan 2006 · 15:04:05")
	}
	formatDuration := func(value time.Duration) string {
		if value <= 0 {
			return "—"
		}
		return value.Round(time.Second).String()
	}
	status := strings.ToUpper(run.Status)
	if status == "" {
		status = "UNKNOWN"
	}
	finished := "running"
	if run.FinishedAt != nil {
		finished = formatTime(*run.FinishedAt)
	}
	maxDuration := time.Duration(0)
	totalTokens := 0
	totalFiles := 0
	for _, task := range run.Tasks {
		if task.Duration > maxDuration {
			maxDuration = task.Duration
		}
		totalTokens += task.Tokens
		totalFiles += len(task.Files)
	}

	var b strings.Builder
	fmt.Fprintln(&b, "EXECUTION SUMMARY")
	fmt.Fprintln(&b, "─────────────────")
	fmt.Fprintf(&b, "Task       %s\n", empty(run.TaskTitle))
	fmt.Fprintf(&b, "Project    %s\n", empty(run.Project))
	fmt.Fprintf(&b, "Status     %s\n", status)
	fmt.Fprintf(&b, "Started    %s\n", formatTime(run.StartedAt))
	fmt.Fprintf(&b, "Finished   %s\n", finished)
	fmt.Fprintf(&b, "Iteration  %s\n", run.ID)
	fmt.Fprintf(&b, "Tasks      %d · %d tokens · %d files\n", len(run.Tasks), totalTokens, totalFiles)

	fmt.Fprintln(&b, "\nTIMELINE")
	fmt.Fprintln(&b, "────────")
	fmt.Fprintf(&b, "● %s  Run started\n", formatTime(run.StartedAt))
	for _, task := range run.Tasks {
		marker := "○"
		switch strings.ToLower(task.Status) {
		case "completed", "done", "success":
			marker = "✓"
		case "failed", "error", "changes_requested":
			marker = "✗"
		}
		fmt.Fprintf(&b, "%s %s  %s\n", marker, formatTime(task.StartedAt), task.Description)
		barLength := 1
		if maxDuration > 0 && task.Duration > 0 {
			barLength = (int(task.Duration) * 24) / int(maxDuration)
			if barLength < 1 {
				barLength = 1
			}
		}
		bar := strings.Repeat("█", barLength) + strings.Repeat("·", 24-barLength)
		statusLabel := strings.ToUpper(task.Status)
		if statusLabel == "" {
			statusLabel = "UNKNOWN"
		}
		fmt.Fprintf(&b, "  └─ %-11s %s  %s\n", statusLabel, bar, formatDuration(task.Duration))
		fmt.Fprintf(&b, "     %s → %s · %d tokens\n", formatTime(task.StartedAt), formatTime(task.FinishedAt), task.Tokens)
		if task.Attempts > 0 || task.ReviewRounds > 0 {
			fmt.Fprintf(&b, "     Attempts: %d · Code reviews: %d\n", task.Attempts, task.ReviewRounds)
		} else {
			fmt.Fprintln(&b, "     Attempts/reviews: not recorded in this iteration")
		}
		if len(task.Files) > 0 {
			fmt.Fprintf(&b, "     Files: %s\n", strings.Join(task.Files, ", "))
		}
		if task.Error != "" {
			fmt.Fprintf(&b, "     Error: %s\n", task.Error)
		}
	}
	if run.FinishedAt != nil {
		fmt.Fprintf(&b, "● %s  Run finished · %s\n", finished, status)
	} else {
		fmt.Fprintln(&b, "○ Run is still in progress")
	}

	fmt.Fprintln(&b, "\nPARAMETERS")
	fmt.Fprintln(&b, "──────────")
	fmt.Fprintf(&b, "Retries    %d\n", run.Config.MaxRetries)
	fmt.Fprintf(&b, "Test       %s\n", empty(run.Config.TestCommand))
	for _, role := range []string{"coder", "reviewer", "judge"} {
		cli := empty(run.Config.Agents[role])
		model := empty(run.Config.Models[role])
		fmt.Fprintf(&b, "%-10s %s · model: %s\n", roleLabel(role), cli, model)
	}

	return b.String()
}

func roleLabel(role string) string {
	switch role {
	case "coder":
		return "Coder"
	case "reviewer":
		return "Reviewer"
	case "judge":
		return "Judge"
	default:
		return role
	}
}

func empty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}
