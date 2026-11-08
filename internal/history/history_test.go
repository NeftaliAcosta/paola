package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neftaliacosta/paola/internal/config"
)

func TestRecorderPersistsOrderedEventsAndAttemptLogs(t *testing.T) {
	root := t.TempDir()
	c := config.Default("demo")
	c.TaskTitle = "traceability"
	recorder, err := Start(root, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetParentExecution("previous-run"); err != nil {
		t.Fatal(err)
	}
	started := nowForTest()
	if err := recorder.RecordTask(TaskRecord{Index: 1, Description: "demo", Status: "running", StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Event(Event{TaskIndex: 1, Task: "demo", Type: "task_started", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.LogAttempt(1, 1, "coder", "first attempt"); err != nil {
		t.Fatal(err)
	}
	if err := recorder.LogAttempt(1, 2, "coder", "second attempt"); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Event(Event{TaskIndex: 1, Task: "demo", Type: "review_decision", Status: "approved", Decision: "STATUS: LGTM"}); err != nil {
		t.Fatal(err)
	}

	runs, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || len(runs[0].Events) != 2 {
		t.Fatalf("runs/events = %d/%d, want 1/2", len(runs), len(runs[0].Events))
	}
	if runs[0].ParentID != "previous-run" {
		t.Fatalf("parent execution = %q, want previous-run", runs[0].ParentID)
	}
	for i, event := range runs[0].Events {
		if event.Sequence != i+1 || event.ID == "" || event.ExecutionID != runs[0].ID {
			t.Fatalf("event %d has invalid identity: %+v", i, event)
		}
	}
	for _, name := range []string{"001-attempt-01-coder.enc", "001-attempt-02-coder.enc"} {
		if _, err := os.Stat(filepath.Join(root, config.Dir, "history", "tasks", "traceability", "iterations", runs[0].ID, "logs", "tasks", name)); err != nil {
			t.Fatalf("missing attempt transcript %q: %v", name, err)
		}
	}
}

func TestMigrateLegacyManifestBuildsAuditTimeline(t *testing.T) {
	m := &Manifest{ID: "legacy", Status: "completed", StartedAt: nowForTest(), Tasks: []TaskRecord{{Index: 1, Description: "demo", Status: "completed", StartedAt: nowForTest(), FinishedAt: nowForTest(), Metrics: map[string]AgentMetric{"coder": {Role: "coder", CLI: "codex", Status: "completed"}}}}}
	if !migrateLegacy(m) {
		t.Fatal("legacy manifest was not migrated")
	}
	if m.SchemaVersion != 2 || len(m.Events) < 4 {
		t.Fatalf("schema/events = %d/%d, want schema 2 and timeline", m.SchemaVersion, len(m.Events))
	}
	if m.Events[2].Type != "agent_summary" || m.Events[len(m.Events)-1].Type != "execution_finished" {
		t.Fatalf("unexpected migrated event sequence: %+v", m.Events)
	}
}

func nowForTest() time.Time { return time.Now().UTC() }
