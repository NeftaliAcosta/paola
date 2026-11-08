package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neftaliacosta/paola/internal/config"
)

func TestExportDefaultContainsMetadataOnlyAndVerifiesEvidence(t *testing.T) {
	root := t.TempDir()
	c := config.Default("demo")
	c.TaskTitle = "export-test"
	r, err := Start(root, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RecordTask(TaskRecord{Index: 1, Description: "demo", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := r.LogAttempt(1, 0, "coder", "token=secret-value"); err != nil {
		t.Fatal(err)
	}
	if err := r.Event(Event{TaskIndex: 1, Type: "agent_finished", OutputRef: "logs/tasks/001-coder.enc"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish("completed"); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "audit")
	if err := Export(root, r.ID(), out); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", "events.json", "integrity.json", "SHA256SUMS"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "logs")); !os.IsNotExist(err) {
		t.Fatal("default export included sensitive logs")
	}
	b, err := os.ReadFile(filepath.Join(out, "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret-value") {
		t.Fatal("secret leaked into metadata export")
	}
}

func TestExportSensitiveIncludesRedactedEvidence(t *testing.T) {
	root := t.TempDir()
	c := config.Default("demo")
	c.TaskTitle = "sensitive-export"
	r, err := Start(root, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RecordTask(TaskRecord{Index: 1, Description: "demo", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := r.LogAttempt(1, 0, "coder", "Bearer abc123"); err != nil {
		t.Fatal(err)
	}
	if err := r.Event(Event{TaskIndex: 1, Type: "agent_finished", OutputRef: "logs/tasks/001-coder.enc"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish("completed"); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "audit")
	if err := ExportWithOptions(root, r.ID(), out, ExportOptions{IncludeSensitive: true}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "logs", "tasks", "001-coder.enc"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "abc123") || !strings.Contains(string(b), "REDACTED") {
		t.Fatalf("sensitive artifact was not redacted: %q", b)
	}
}

func TestExportRejectsMissingEvidenceAndDoesNotWriteChecksums(t *testing.T) {
	root := t.TempDir()
	c := config.Default("demo")
	c.TaskTitle = "missing-export"
	r, err := Start(root, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Event(Event{TaskIndex: 1, Type: "agent_finished", OutputRef: "logs/tasks/001-missing.enc"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish("failed"); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "audit")
	if err := Export(root, r.ID(), out); err == nil {
		t.Fatal("missing encrypted evidence was silently accepted")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("failed export left destination behind: %v", statErr)
	}
}

func TestValidateArtifactRefRejectsTraversalAndSymlink(t *testing.T) {
	base := t.TempDir()
	for _, ref := range []string{"../../secret", "/etc/passwd", "logs/tasks/../../../secret", "logs\\tasks\\001.enc", ""} {
		if _, err := validateArtifactRef(base, ref); err == nil {
			t.Errorf("reference %q was accepted", ref)
		}
	}
	if err := os.MkdirAll(filepath.Join(base, "logs", "tasks"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "outside.enc")
	if err := os.WriteFile(target, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "logs", "tasks", "link.enc")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := validateArtifactRef(base, "logs/tasks/link.enc"); err == nil {
		t.Fatal("symlink artifact was accepted")
	}
}

func TestExportRejectsOccupiedDestination(t *testing.T) {
	root := t.TempDir()
	c := config.Default("demo")
	c.TaskTitle = "occupied-export"
	r, err := Start(root, c)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "audit")
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Export(root, r.ID(), out); err == nil {
		t.Fatal("occupied destination was accepted")
	}
}
