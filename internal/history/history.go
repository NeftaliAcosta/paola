package history

import (
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/neftaliacosta/paola/internal/config"
)

type Manifest struct {
	SchemaVersion int           `json:"schema_version,omitempty"`
	ID            string        `json:"id"`
	ParentID      string        `json:"parent_execution_id,omitempty"`
	TaskTitle     string        `json:"task_title"`
	StartedAt     time.Time     `json:"started_at"`
	FinishedAt    *time.Time    `json:"finished_at,omitempty"`
	Status        string        `json:"status"`
	Project       string        `json:"project"`
	Config        config.Config `json:"config"`
	Tasks         []TaskRecord  `json:"tasks,omitempty"`
	Events        []Event       `json:"events,omitempty"`
}

// Event is an append-only audit record for one orchestration transition.
// It intentionally stores references to encrypted transcripts instead of
// duplicating agent output in the manifest.
type Event struct {
	ID          string    `json:"id"`
	Sequence    int       `json:"sequence"`
	ExecutionID string    `json:"execution_id"`
	TaskIndex   int       `json:"task_index,omitempty"`
	Task        string    `json:"task,omitempty"`
	Attempt     int       `json:"attempt,omitempty"`
	Role        string    `json:"role,omitempty"`
	Agent       string    `json:"agent,omitempty"`
	Type        string    `json:"type"`
	Status      string    `json:"status,omitempty"`
	Decision    string    `json:"decision,omitempty"`
	Error       string    `json:"error,omitempty"`
	OutputRef   string    `json:"output_ref,omitempty"`
	Evidence    []string  `json:"evidence,omitempty"`
	Synthetic   bool      `json:"synthetic,omitempty"`
	At          time.Time `json:"at"`
}
type TaskRecord struct {
	Index        int                    `json:"index"`
	Description  string                 `json:"description"`
	Status       string                 `json:"status"`
	StartedAt    time.Time              `json:"started_at"`
	FinishedAt   time.Time              `json:"finished_at"`
	Duration     time.Duration          `json:"duration_ns"`
	Attempts     int                    `json:"attempts,omitempty"`
	ReviewRounds int                    `json:"review_rounds,omitempty"`
	Tokens       int                    `json:"tokens,omitempty"`
	Files        []string               `json:"files,omitempty"`
	Error        string                 `json:"error,omitempty"`
	Metrics      map[string]AgentMetric `json:"metrics,omitempty"`
}

type AgentMetric struct {
	Role            string        `json:"role"`
	CLI             string        `json:"cli"`
	Model           string        `json:"model,omitempty"`
	Effort          string        `json:"effort,omitempty"`
	Turns           int           `json:"turns"`
	InputChars      int           `json:"input_chars"`
	OutputChars     int           `json:"output_chars"`
	InputTokens     int           `json:"input_tokens,omitempty"`
	OutputTokens    int           `json:"output_tokens,omitempty"`
	EstimatedTokens int           `json:"estimated_tokens,omitempty"`
	Duration        time.Duration `json:"duration_ns"`
	Status          string        `json:"status"`
	Retryable       bool          `json:"retryable,omitempty"`
	Error           string        `json:"error,omitempty"`
	SessionID       string        `json:"session_id,omitempty"`
	ContextMode     string        `json:"context_mode,omitempty"`
	SessionResumed  bool          `json:"session_resumed,omitempty"`
	FallbackUsed    bool          `json:"session_fallback_used,omitempty"`
}

type TestEvidence struct {
	Command    string        `json:"command"`
	Workdir    string        `json:"workdir"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Duration   time.Duration `json:"duration_ns"`
	ExitCode   int           `json:"exit_code"`
	Status     string        `json:"status"`
	Output     string        `json:"output"`
}
type Recorder struct {
	root     string
	key      []byte
	manifest Manifest
	dir      string
}

func Start(root string, c config.Config) (*Recorder, error) {
	key, err := loadKey(root)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", c.TaskTitle, now.UnixNano())))
	id := now.Format("20060102T150405Z") + "-" + hex.EncodeToString(sum[:])[:8]
	dir := filepath.Join(root, config.Dir, "history", "tasks", safeTitle(c.TaskTitle), "iterations", id)
	if err := os.MkdirAll(filepath.Join(dir, "logs", "tasks"), 0700); err != nil {
		return nil, err
	}
	r := &Recorder{root: root, key: key, dir: dir, manifest: Manifest{SchemaVersion: 2, ID: id, TaskTitle: nonEmpty(c.TaskTitle, "Untitled task"), StartedAt: now, Status: "running", Project: c.ProjectName, Config: c}}
	return r, r.save()
}
func (r *Recorder) ID() string { return r.manifest.ID }

// SetParentExecution links a resumed run to the prior execution without
// mutating the prior encrypted manifest. A new manifest remains the durable
// boundary for every invocation, while the lineage stays queryable.
func (r *Recorder) SetParentExecution(id string) error {
	r.manifest.ParentID = strings.TrimSpace(id)
	return r.save()
}
func (r *Recorder) save() error {
	b, err := json.MarshalIndent(r.manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeEncrypted(filepath.Join(r.dir, "manifest.enc"), r.key, b)
}
func (r *Recorder) RecordTask(t TaskRecord) error {
	r.manifest.Tasks = append(r.manifest.Tasks, t)
	return r.save()
}
func (r *Recorder) UpdateTask(index int, status, errText string, tokens int, files []string, attempts, reviewRounds int) error {
	for i := range r.manifest.Tasks {
		if r.manifest.Tasks[i].Index == index {
			now := time.Now().UTC()
			r.manifest.Tasks[i].Status = status
			r.manifest.Tasks[i].FinishedAt = now
			r.manifest.Tasks[i].Duration = now.Sub(r.manifest.Tasks[i].StartedAt)
			r.manifest.Tasks[i].Error = errText
			r.manifest.Tasks[i].Tokens = tokens
			r.manifest.Tasks[i].Files = files
			r.manifest.Tasks[i].Attempts = attempts
			r.manifest.Tasks[i].ReviewRounds = reviewRounds
			return r.save()
		}
	}
	return nil
}

func (r *Recorder) UpdateTaskMetrics(index int, metrics map[string]AgentMetric) error {
	for i := range r.manifest.Tasks {
		if r.manifest.Tasks[i].Index == index {
			r.manifest.Tasks[i].Metrics = metrics
			return r.save()
		}
	}
	return nil
}

// Event appends one durable audit event and immediately persists the
// manifest. A failed write is returned to the caller so orchestration cannot
// silently continue with incomplete audit evidence.
func (r *Recorder) Event(event Event) error {
	event.Sequence = len(r.manifest.Events) + 1
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if event.ExecutionID == "" {
		event.ExecutionID = r.manifest.ID
	}
	event.ID = fmt.Sprintf("%s-%06d", r.manifest.ID, event.Sequence)
	event.Error = trimError(event.Error)
	r.manifest.Events = append(r.manifest.Events, event)
	return r.save()
}
func (r *Recorder) Finish(status string) error {
	return r.FinishWithError(status, "")
}

func (r *Recorder) FinishWithError(status, errorText string) error {
	now := time.Now().UTC()
	for i := range r.manifest.Tasks {
		if r.manifest.Tasks[i].Status == "running" {
			r.manifest.Tasks[i].Status = status
			r.manifest.Tasks[i].FinishedAt = now
			r.manifest.Tasks[i].Duration = now.Sub(r.manifest.Tasks[i].StartedAt)
			if status != "completed" {
				r.manifest.Tasks[i].Error = errorText
				if r.manifest.Tasks[i].Error == "" {
					r.manifest.Tasks[i].Error = "run ended before task completion"
				}
			}
		}
	}
	r.manifest.FinishedAt = &now
	r.manifest.Status = status
	return r.save()
}
func (r *Recorder) Log(taskIndex int, role, output string) error {
	return r.LogAttempt(taskIndex, 0, role, output)
}

// LogAttempt keeps every role transcript, including repeated attempts.
func (r *Recorder) LogAttempt(taskIndex, attempt int, role, output string) error {
	suffix := role
	if attempt > 0 {
		suffix = fmt.Sprintf("attempt-%02d-%s", attempt, role)
	}
	p := filepath.Join(r.dir, "logs", "tasks", fmt.Sprintf("%03d-%s.enc", taskIndex, suffix))
	return writeEncrypted(p, r.key, []byte(output))
}
func (r *Recorder) Diff(taskIndex int, diff string) error {
	return r.DiffAttempt(taskIndex, 0, diff)
}

// DiffAttempt preserves a separate encrypted diff for every task attempt.
// Attempt zero keeps compatibility with older callers and history readers.
func (r *Recorder) DiffAttempt(taskIndex, attempt int, diff string) error {
	if strings.TrimSpace(diff) == "" {
		return nil
	}
	name := fmt.Sprintf("task-%03d.patch.enc", taskIndex)
	if attempt > 0 {
		name = fmt.Sprintf("task-%03d-attempt-%02d.patch.enc", taskIndex, attempt)
	}
	p := filepath.Join(r.dir, "diffs", name)
	return writeEncrypted(p, r.key, []byte(diff))
}

// LogTestEvidence stores test output per task attempt instead of reusing the
// mutable state/test_output.log file as historical evidence.
func (r *Recorder) LogTestEvidence(taskIndex, attempt int, evidence TestEvidence) (string, error) {
	ref := filepath.Join("evidence", "tasks", fmt.Sprintf("%03d", taskIndex), fmt.Sprintf("attempt-%02d-tests.enc", attempt))
	b, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return ref, err
	}
	return ref, writeEncrypted(filepath.Join(r.dir, ref), r.key, b)
}

func List(root string) ([]Manifest, error) {
	key, err := loadKey(root)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(root, config.Dir, "history", "tasks")
	var out []Manifest
	err = filepath.WalkDir(base, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() || d.Name() != "manifest.enc" {
			return nil
		}
		b, err := readEncrypted(path, key)
		if err == nil {
			var m Manifest
			if json.Unmarshal(b, &m) == nil && len(m.Tasks) > 0 {
				if migrateLegacy(&m) {
					if updated, marshalErr := json.MarshalIndent(m, "", "  "); marshalErr == nil {
						_ = writeEncrypted(path, key, updated)
					}
				}
				out = append(out, m)
			}
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return out, err
}

func migrateLegacy(m *Manifest) bool {
	if m.SchemaVersion >= 2 && len(m.Events) > 0 {
		return false
	}
	if m.SchemaVersion == 0 {
		m.SchemaVersion = 2
	}
	if len(m.Events) > 0 {
		return true
	}
	sequence := 0
	add := func(taskIndex int, task, eventType, status, errText string, at time.Time) {
		sequence++
		m.Events = append(m.Events, Event{ID: fmt.Sprintf("%s-%06d", m.ID, sequence), Sequence: sequence, ExecutionID: m.ID, TaskIndex: taskIndex, Task: task, Type: eventType, Status: status, Error: trimError(errText), Synthetic: true, At: at})
	}
	add(0, "", "execution_started", "running", "", m.StartedAt)
	for _, task := range m.Tasks {
		add(task.Index, task.Description, "task_started", "running", "", task.StartedAt)
		for _, role := range []string{"coder", "reviewer", "judge"} {
			metric, ok := task.Metrics[role]
			if !ok {
				continue
			}
			add(task.Index, task.Description, "agent_summary", metric.Status, metric.Error, task.FinishedAt)
			m.Events[len(m.Events)-1].Role = role
			m.Events[len(m.Events)-1].Agent = metric.CLI
		}
		status := task.Status
		if status == "" {
			status = "unknown"
		}
		add(task.Index, task.Description, "task_finished", status, task.Error, task.FinishedAt)
	}
	add(0, "", "execution_finished", m.Status, "", valueTime(m.FinishedAt, m.StartedAt))
	return true
}

func valueTime(value *time.Time, fallback time.Time) time.Time {
	if value != nil {
		return *value
	}
	return fallback
}

func trimError(value string) string {
	value = strings.ToValidUTF8(strings.TrimSpace(value), "�")
	if len(value) > 2000 {
		return value[:2000] + "…"
	}
	return value
}
func Load(root, id string) (Manifest, error) {
	runs, err := List(root)
	if err != nil {
		return Manifest{}, err
	}
	for _, m := range runs {
		if m.ID == id {
			return m, nil
		}
	}
	return Manifest{}, fmt.Errorf("history iteration %q not found", id)
}

// LoadDiff returns the encrypted diff captured for one task in an iteration.
func LoadDiff(root string, manifest Manifest, taskIndex int) (string, error) {
	key, err := loadKey(root)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, config.Dir, "history", "tasks", safeTitle(manifest.TaskTitle), "iterations", manifest.ID, "diffs")
	paths, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("task-%03d-attempt-*.patch.enc", taskIndex)))
	if len(paths) > 0 {
		b, readErr := readEncrypted(paths[len(paths)-1], key)
		if readErr == nil {
			return string(b), nil
		}
	}
	return LoadDiffAttempt(root, manifest, taskIndex, 0)
}

// LoadDiffAttempt reads the attempt-specific diff and falls back to the
// legacy task-only path for manifests written before attempt isolation.
func LoadDiffAttempt(root string, manifest Manifest, taskIndex, attempt int) (string, error) {
	key, err := loadKey(root)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("task-%03d.patch.enc", taskIndex)
	if attempt > 0 {
		name = fmt.Sprintf("task-%03d-attempt-%02d.patch.enc", taskIndex, attempt)
	}
	p := filepath.Join(root, config.Dir, "history", "tasks", safeTitle(manifest.TaskTitle), "iterations", manifest.ID, "diffs", name)
	b, err := readEncrypted(p, key)
	if err != nil {
		if attempt > 0 && os.IsNotExist(err) {
			return LoadDiff(root, manifest, taskIndex)
		}
		return "", err
	}
	return string(b), nil
}

func loadKey(root string) ([]byte, error) {
	if secret := os.Getenv("PAOLA_SECRET_KEY"); secret != "" {
		key := sha256.Sum256([]byte(secret))
		return key[:], nil
	}
	p := filepath.Join(root, config.Dir, "history", ".key")
	if b, err := os.ReadFile(p); err == nil && len(b) == 32 {
		restrictKeyFile(p)
		return b, nil
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	} else if err == nil {
		return nil, fmt.Errorf("invalid history key file %q: expected 32 bytes", p)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return nil, err
	}
	// O_EXCL prevents two concurrent paola processes from generating different
	// keys for the same workspace. Windows uses the same atomic create idiom;
	// its default ACLs, rather than POSIX mode bits, control access there.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			if existing, readErr := os.ReadFile(p); readErr == nil && len(existing) == 32 {
				restrictKeyFile(p)
				return existing, nil
			}
		}
		return nil, err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	restrictKeyFile(p)
	return b, nil
}

func restrictKeyFile(path string) {
	if runtime.GOOS != "windows" {
		// chmod is best-effort: the key is still usable if a filesystem does not
		// support POSIX mode bits, while Windows relies on the file ACL.
		_ = os.Chmod(path, 0600)
	}
}
func writeEncrypted(path string, key, data []byte) error {
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	var compressed strings.Builder
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(data); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(compressed.String()), nil)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".paola-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(sealed); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
func readEncrypted(path string, key []byte) ([]byte, error) {
	sealed, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	n := gcm.NonceSize()
	if len(sealed) < n {
		return nil, fmt.Errorf("invalid encrypted history file")
	}
	compressed, err := gcm.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return nil, fmt.Errorf("history integrity check failed: %w", err)
	}
	gz, err := gzip.NewReader(strings.NewReader(string(compressed)))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	return io.ReadAll(gz)
}
func safeTitle(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "untitled-task"
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() == 0 || !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	v := strings.Trim(b.String(), "-")
	if len(v) > 64 {
		v = v[:64]
	}
	if v == "" {
		return "untitled-task"
	}
	return v
}
func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return strings.TrimSpace(s)
}
