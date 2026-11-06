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
	ID         string        `json:"id"`
	TaskTitle  string        `json:"task_title"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt *time.Time    `json:"finished_at,omitempty"`
	Status     string        `json:"status"`
	Project    string        `json:"project"`
	Config     config.Config `json:"config"`
	Tasks      []TaskRecord  `json:"tasks,omitempty"`
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
	Role        string        `json:"role"`
	CLI         string        `json:"cli"`
	Model       string        `json:"model,omitempty"`
	Effort      string        `json:"effort,omitempty"`
	Turns       int           `json:"turns"`
	InputChars  int           `json:"input_chars"`
	OutputChars int           `json:"output_chars"`
	Duration    time.Duration `json:"duration_ns"`
	Status      string        `json:"status"`
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
	if err := os.MkdirAll(filepath.Join(dir, "logs", "tasks"), 0755); err != nil {
		return nil, err
	}
	r := &Recorder{root: root, key: key, dir: dir, manifest: Manifest{ID: id, TaskTitle: nonEmpty(c.TaskTitle, "Untitled task"), StartedAt: now, Status: "running", Project: c.ProjectName, Config: c}}
	return r, r.save()
}
func (r *Recorder) ID() string { return r.manifest.ID }
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
	if strings.TrimSpace(output) == "" {
		return nil
	}
	p := filepath.Join(r.dir, "logs", "tasks", fmt.Sprintf("%03d-%s.enc", taskIndex, role))
	return writeEncrypted(p, r.key, []byte(output))
}
func (r *Recorder) Diff(taskIndex int, diff string) error {
	if strings.TrimSpace(diff) == "" {
		return nil
	}
	p := filepath.Join(r.dir, "diffs", fmt.Sprintf("task-%03d.patch.enc", taskIndex))
	return writeEncrypted(p, r.key, []byte(diff))
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
	p := filepath.Join(root, config.Dir, "history", "tasks", safeTitle(manifest.TaskTitle), "iterations", manifest.ID, "diffs", fmt.Sprintf("task-%03d.patch.enc", taskIndex))
	b, err := readEncrypted(p, key)
	if err != nil {
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
	return os.WriteFile(path, sealed, 0600)
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
