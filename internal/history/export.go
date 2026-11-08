package history

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/neftaliacosta/paola/internal/config"
)

var sensitiveExportRE = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+|((?:api[_-]?key|token|secret|password)\s*[:=]\s*)[^\s,;]+`)

type ExportOptions struct {
	IncludeSensitive bool
}

// Export writes the safe, metadata-only audit bundle.
func Export(root, id, output string) error {
	return ExportWithOptions(root, id, output, ExportOptions{})
}

// ExportWithOptions validates every referenced encrypted artifact. Sensitive
// mode additionally decrypts artifacts into a redacted, disposable bundle.
func ExportWithOptions(root, id, output string, options ExportOptions) (err error) {
	if strings.TrimSpace(output) == "" {
		return fmt.Errorf("export output is required")
	}
	if err := rejectSymlinkComponents(output); err != nil {
		return fmt.Errorf("invalid export destination: %w", err)
	}
	if _, err := os.Lstat(output); err == nil {
		return fmt.Errorf("export destination already exists: %s", output)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Mkdir(output, 0700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(output)
		}
	}()

	run, err := Load(root, id)
	if err != nil {
		return err
	}
	key, err := loadKey(root)
	if err != nil {
		return err
	}
	base := filepath.Join(root, config.Dir, "history", "tasks", safeTitle(run.TaskTitle), "iterations", run.ID)
	if err := rejectSymlinkComponents(base); err != nil {
		return fmt.Errorf("invalid execution history: %w", err)
	}

	files := map[string][]byte{}
	manifest, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	events, err := json.MarshalIndent(run.Events, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal events: %w", err)
	}
	files["manifest.json"] = []byte(redact(string(manifest)))
	files["events.json"] = []byte(redact(string(events)))

	references := make(map[string]struct{})
	for _, event := range run.Events {
		refs := append([]string{}, event.Evidence...)
		if event.OutputRef != "" {
			refs = append(refs, event.OutputRef)
		}
		for _, ref := range refs {
			if !isEncryptedReference(ref) {
				if hasUnsafePathSyntax(ref) {
					return fmt.Errorf("unsafe evidence reference %q", ref)
				}
				continue
			}
			if event.Synthetic {
				continue
			}
			clean, err := validateArtifactRef(base, ref)
			if err != nil {
				return err
			}
			if _, err := readEncrypted(filepath.Join(base, filepath.FromSlash(clean)), key); err != nil {
				return fmt.Errorf("verify encrypted evidence %q: %w", ref, err)
			}
			references[clean] = struct{}{}
		}
	}

	if options.IncludeSensitive {
		for _, ref := range sortedKeys(references) {
			data, err := readEncrypted(filepath.Join(base, filepath.FromSlash(ref)), key)
			if err != nil {
				return fmt.Errorf("read encrypted evidence %q: %w", ref, err)
			}
			files[ref] = []byte(redact(string(data)))
		}
	}

	integrity := struct {
		SchemaVersion    int      `json:"schema_version"`
		ExecutionID      string   `json:"execution_id"`
		VerifiedArtifact []string `json:"verified_artifacts,omitempty"`
		Sensitive        bool     `json:"include_sensitive"`
		GeneratedAt      string   `json:"generated_at"`
	}{1, run.ID, sortedKeys(references), options.IncludeSensitive, time.Now().UTC().Format(time.RFC3339Nano)}
	integrityBytes, err := json.MarshalIndent(integrity, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal integrity metadata: %w", err)
	}
	files["integrity.json"] = integrityBytes

	if err := writeExportFiles(output, files); err != nil {
		return err
	}
	return writeChecksums(output, files)
}

func isEncryptedReference(ref string) bool {
	ref = filepath.ToSlash(ref)
	return strings.HasPrefix(ref, "logs/tasks/") || strings.HasPrefix(ref, "diffs/") || strings.HasPrefix(ref, "evidence/tasks/")
}

func hasUnsafePathSyntax(ref string) bool {
	return filepath.IsAbs(ref) || strings.ContainsRune(ref, '\x00') || strings.Contains(ref, "..") || strings.Contains(ref, "\\")
}

func validateArtifactRef(base, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" || hasUnsafePathSyntax(ref) {
		return "", fmt.Errorf("unsafe encrypted artifact reference %q", ref)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(ref)))
	if clean != ref || !isEncryptedReference(clean) {
		return "", fmt.Errorf("unsupported encrypted artifact reference %q", ref)
	}
	full := filepath.Join(base, filepath.FromSlash(clean))
	rel, err := filepath.Rel(base, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("artifact reference escapes execution %q", ref)
	}
	if err := rejectSymlinkComponents(full); err != nil {
		return "", fmt.Errorf("artifact reference %q: %w", ref, err)
	}
	return clean, nil
}

func rejectSymlinkComponents(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	rest := strings.TrimPrefix(abs, volume)
	current := volume + string(filepath.Separator)
	for _, component := range strings.Split(rest, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if runtime.GOOS == "darwin" && current == "/var" {
				// macOS exposes /var as a system symlink to /private/var.
				// It is outside the user-controlled export path.
				continue
			}
			return fmt.Errorf("symlink component is not allowed: %s", current)
		}
	}
	return nil
}

func writeExportFiles(output string, files map[string][]byte) error {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		target := filepath.Join(output, filepath.FromSlash(path))
		if err := rejectSymlinkComponents(filepath.Dir(target)); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(target, files[path], 0600); err != nil {
			return fmt.Errorf("write export file %q: %w", path, err)
		}
	}
	return nil
}

func writeChecksums(output string, files map[string][]byte) error {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var sums strings.Builder
	for _, path := range paths {
		sum := sha256.Sum256(files[path])
		sums.WriteString(hex.EncodeToString(sum[:]) + "  " + path + "\n")
	}
	return os.WriteFile(filepath.Join(output, "SHA256SUMS"), []byte(sums.String()), 0600)
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func redact(value string) string {
	return sensitiveExportRE.ReplaceAllString(value, `${1}${2}[REDACTED]`)
}
