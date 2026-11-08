package gitutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureDiffIncludesStagedProjectChanges(t *testing.T) {
	root := t.TempDir()
	gitTestRun(t, root, "init", "-q")
	gitTestRun(t, root, "config", "user.name", "PAOLA test")
	gitTestRun(t, root, "config", "user.email", "paola-test@example.invalid")

	if err := writeTestFile(filepath.Join(root, "README.md"), "initial\n"); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, root, "add", "README.md")
	gitTestRun(t, root, "commit", "-q", "-m", "initial")

	if err := writeTestFile(filepath.Join(root, "report.md"), "evidence\n"); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, root, "add", "report.md")

	diff, err := CaptureDiff(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "report.md") || !strings.Contains(diff, "+evidence") {
		t.Fatalf("staged project change missing from captured diff:\n%s", diff)
	}

	files := ChangedFiles(root)
	if len(files) != 1 || files[0] != "report.md" {
		t.Fatalf("unexpected changed files: %#v", files)
	}
}

func TestCaptureDiffSinceIncludesAgentCommit(t *testing.T) {
	root := t.TempDir()
	gitTestRun(t, root, "init", "-q")
	gitTestRun(t, root, "config", "user.name", "PAOLA test")
	gitTestRun(t, root, "config", "user.email", "paola-test@example.invalid")
	if err := writeTestFile(filepath.Join(root, "README.md"), "initial\n"); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, root, "add", "README.md")
	gitTestRun(t, root, "commit", "-q", "-m", "initial")
	base, err := Head(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(filepath.Join(root, "report.md"), "agent evidence\n"); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, root, "add", "report.md")
	gitTestRun(t, root, "commit", "-q", "-m", "agent report")

	diff, err := CaptureDiffSince(root, base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "report.md") || !strings.Contains(diff, "+agent evidence") {
		t.Fatalf("agent commit missing from captured diff:\n%s", diff)
	}
}

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0600)
}

func gitTestRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, output)
	}
}
