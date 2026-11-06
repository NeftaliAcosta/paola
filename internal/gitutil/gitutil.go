package gitutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

type BranchInfo struct {
	Base string
	Task string
}

// CreateExecutionWorktree creates an isolated branch and worktree without
// touching the caller's checkout. The caller owns the returned directory.
func CreateExecutionWorktree(root, title, baseBranch string) (string, string, error) {
	// Nanoseconds avoid collisions when a user starts two isolated runs within
	// the same second.
	id := time.Now().UTC().Format("20060102T150405.000000000Z")
	branch := "paola/run-" + slug(title) + "-" + strings.ToLower(id)
	if strings.HasSuffix(branch, "-") {
		branch += "run"
	}
	baseDir := filepath.Join(filepath.Dir(root), filepath.Base(root)+".paola-worktrees")
	path := filepath.Join(baseDir, id)
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(baseBranch) == "" {
		baseBranch = "HEAD"
	}
	if output, err := run(root, "worktree", "add", "-b", branch, path, baseBranch); err != nil {
		return "", "", fmt.Errorf("create execution worktree: %s", strings.TrimSpace(string(output)))
	}
	// Workspace metadata remains in the original project and is shared through
	// a symlink. Git ignores .paola, while history and encryption stay unified.
	if err := os.Symlink(filepath.Join(root, ".paola"), filepath.Join(path, ".paola")); err != nil {
		_, _ = run(root, "worktree", "remove", "--force", path)
		return "", "", fmt.Errorf("link PAOLA workspace into worktree: %w", err)
	}
	return path, branch, nil
}

func run(root string, args ...string) ([]byte, error) {
	c := exec.Command("git", args...)
	c.Dir = root
	return c.CombinedOutput()
}
func CaptureDiff(root string) (string, error) {
	// Ask Git for untracked, non-ignored files instead of using `git add -N .`.
	// The latter still visits ignored directories and fails when .paola/ is
	// correctly excluded through .git/info/exclude.
	b, err := run(root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	paths := make([]string, 0)
	for _, path := range strings.Split(string(b), "\x00") {
		if path == "" || path == ".paola" || strings.HasPrefix(path, ".paola/") {
			continue
		}
		paths = append(paths, path)
	}
	if len(paths) > 0 {
		args := append([]string{"add", "-N", "--"}, paths...)
		if _, err := run(root, args...); err != nil {
			return "", err
		}
	}
	b, err = run(root, "diff", "--", ".", ":(exclude).paola/**")
	return string(b), err
}
func Commit(root, task string) error {
	// .paola/ is excluded by Git's local exclude file. Git add -A respects that
	// rule without traversing the ignored workspace as an explicit pathspec.
	if _, err := run(root, "add", "-A", "--", "."); err != nil {
		return err
	}
	_, err := run(root, "commit", "-m", semanticCommitMessage(task))
	return err
}
func IsRepo(root string) bool         { _, err := run(root, "rev-parse", "--show-toplevel"); return err == nil }
func SafeDiffPath(root string) string { return filepath.Join(root, ".paola", "state", "current.diff") }

// EnsureLocalExclude keeps the local Paola workspace out of this repository's
// status without modifying the project's shared .gitignore file.
func EnsureLocalExclude(root string) error {
	b, err := run(root, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	path := strings.TrimSpace(string(b))
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := string(data)
	hasEntry := false
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == ".paola/" || strings.TrimSpace(line) == ".paola" {
			hasEntry = true
			break
		}
	}
	if !hasEntry {
		entry := "# Paola local workspace\n.paola/\n"
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if err := os.WriteFile(path, []byte(content+entry), 0644); err != nil {
			return err
		}
	}
	return cleanLocalPaolaIndex(root)
}

// TrackedLocalWorkspace reports workspace files that are already tracked by
// Git. Local exclude rules cannot affect files that were committed previously.
func TrackedLocalWorkspace(root string) ([]string, error) {
	b, err := run(root, "ls-files", "-z", "--", ".paola/")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, path := range strings.Split(string(b), "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// UntrackLocalWorkspace removes .paola/ from Git's index without deleting
// the local workspace. The resulting index change is intentionally left for
// the user to review and commit; PAOLA must never create a project commit.
func UntrackLocalWorkspace(root string) error {
	if _, err := run(root, "rm", "--cached", "-r", "--ignore-unmatch", "--", ".paola/"); err != nil {
		return err
	}
	return nil
}

// cleanLocalPaolaIndex removes staged additions and empty intent-to-add
// placeholders for the tool-owned workspace. It never removes working-tree
// files or changes existing commits.
func cleanLocalPaolaIndex(root string) error {
	staged, err := run(root, "diff", "--cached", "--name-only", "-z", "--diff-filter=A", "--", ".paola/")
	if err != nil {
		return err
	}
	for _, path := range strings.Split(string(staged), "\x00") {
		if path == "" {
			continue
		}
		if output, err := run(root, "reset", "--quiet", "--", path); err != nil {
			return fmt.Errorf("unstage local Git index entry %q: %s", path, strings.TrimSpace(string(output)))
		}
	}

	b, err := run(root, "ls-files", "--stage", "--", ".paola/")
	if err != nil {
		return err
	}
	const emptyBlob = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		metadata := strings.Fields(parts[0])
		if len(metadata) < 2 || metadata[1] != emptyBlob {
			continue
		}
		if output, err := run(root, "update-index", "--remove", "--", parts[1]); err != nil {
			return fmt.Errorf("remove local Git index entry %q: %s", parts[1], strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func CurrentBranch(root string) (string, error) {
	b, err := run(root, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("unable to determine current Git branch: %s", strings.TrimSpace(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}

func Branches(root string) ([]string, error) {
	b, err := run(root, "branch", "--list")
	if err != nil {
		return nil, fmt.Errorf("unable to list Git branches: %s", strings.TrimSpace(string(b)))
	}
	var branches []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*"))
		if line != "" {
			branches = append(branches, line)
		}
	}
	return branches, nil
}

func HasBranch(root, branch string) bool {
	_, err := run(root, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func CheckoutTaskBranch(root, base, task string) error {
	if current, err := CurrentBranch(root); err == nil && current == task {
		return nil
	}
	if output, err := run(root, "checkout", base); err != nil {
		return fmt.Errorf("unable to checkout base branch %q: %s", base, strings.TrimSpace(string(output)))
	}
	if task == base {
		return nil
	}
	if HasBranch(root, task) {
		if output, err := run(root, "checkout", task); err != nil {
			return fmt.Errorf("unable to checkout task branch %q: %s", task, strings.TrimSpace(string(output)))
		}
		return nil
	}
	if output, err := run(root, "checkout", "-b", task, base); err != nil {
		return fmt.Errorf("unable to create task branch %q: %s", task, strings.TrimSpace(string(output)))
	}
	return nil
}

func TaskBranch(title string) string {
	branch := slug(title)
	if branch == "" {
		branch = "untitled-task"
	}
	return "paola/" + branch
}

func CleanTask(s string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "- [ ]"), "- [x]"))
}

func slug(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func semanticCommitMessage(task string) string {
	clean := strings.Join(strings.Fields(strings.TrimSpace(task)), " ")
	lower := strings.ToLower(clean)
	typeName := "chore"
	switch {
	case strings.Contains(lower, "fix"), strings.Contains(lower, "bug"), strings.Contains(lower, "correg"), strings.Contains(lower, "repar"):
		typeName = "fix"
	case strings.Contains(lower, "test"), strings.Contains(lower, "prueba"):
		typeName = "test"
	case strings.Contains(lower, "refactor"), strings.Contains(lower, "restruct"):
		typeName = "refactor"
	case strings.Contains(lower, "doc"):
		typeName = "docs"
	case strings.Contains(lower, "crear"), strings.Contains(lower, "implementar"), strings.Contains(lower, "agregar"), strings.Contains(lower, "añadir"), strings.Contains(lower, "add"), strings.Contains(lower, "implement"):
		typeName = "feat"
	}
	scope := "project"
	for _, candidate := range []struct {
		words []string
		value string
	}{
		{[]string{"usuario", "usuarios", "user", "users"}, "users"},
		{[]string{"producto", "productos", "product", "products"}, "products"},
		{[]string{"autenticacion", "autenticación", "auth", "login"}, "auth"},
		{[]string{"docker", "contenedor", "contenedores"}, "docker"},
	} {
		for _, word := range candidate.words {
			if strings.Contains(lower, word) {
				scope = candidate.value
				break
			}
		}
		if scope != "project" {
			break
		}
	}
	clean = strings.ReplaceAll(strings.ToLower(clean), "paola", "")
	clean = strings.TrimSpace(strings.Trim(clean, "-:;,"))
	if clean == "" {
		clean = "update project"
	}
	return fmt.Sprintf("%s(%s): %s", typeName, scope, strings.ToLower(clean))
}

func ChangedFiles(root string) []string {
	b, err := run(root, "diff", "--name-only", "--", ".", ":(exclude).paola/**")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			files = append(files, strings.TrimSpace(line))
		}
	}
	return files
}
