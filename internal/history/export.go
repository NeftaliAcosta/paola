package history

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/neftaliacosta/paola/internal/config"
)

var sensitiveExportRE = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+|((?:api[_-]?key|token|secret|password)\s*[:=]\s*)[^\s,;]+`)

// Export writes a redacted, integrity-checkable audit bundle. Encrypted source
// history remains untouched; the export is an explicit user action.
func Export(root, id, output string) error {
	run, err := Load(root, id)
	if err != nil {
		return err
	}
	key, err := loadKey(root)
	if err != nil {
		return err
	}
	if strings.TrimSpace(output) == "" {
		return fmt.Errorf("export output is required")
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		return err
	}
	files := map[string][]byte{}
	manifest, _ := json.MarshalIndent(run, "", "  ")
	files["manifest.json"] = []byte(redact(string(manifest)))
	events, _ := json.MarshalIndent(run.Events, "", "  ")
	files["events.json"] = []byte(redact(string(events)))
	base := filepath.Join(root, config.Dir, "history", "tasks", safeTitle(run.TaskTitle), "iterations", run.ID)
	for _, event := range run.Events {
		for _, ref := range append([]string{event.OutputRef}, event.Evidence...) {
			if ref == "" {
				continue
			}
			data, readErr := readEncrypted(filepath.Join(base, ref), key)
			if readErr != nil {
				continue
			}
			files[filepath.ToSlash(ref)] = []byte(redact(string(data)))
		}
	}
	for _, pattern := range []string{"diffs/*.enc", "logs/tasks/*.enc", "evidence/tasks/**/*.enc"} {
		paths, _ := filepath.Glob(filepath.Join(base, pattern))
		for _, path := range paths {
			data, readErr := readEncrypted(path, key)
			if readErr != nil {
				continue
			}
			rel, _ := filepath.Rel(base, path)
			files[filepath.ToSlash(rel)] = []byte(redact(string(data)))
		}
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var sums strings.Builder
	for _, path := range paths {
		target := filepath.Join(output, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(target, files[path], 0600); err != nil {
			return err
		}
		sum := sha256.Sum256(files[path])
		sums.WriteString(hex.EncodeToString(sum[:]) + "  " + path + "\n")
	}
	return os.WriteFile(filepath.Join(output, "SHA256SUMS"), []byte(sums.String()), 0600)
}

func redact(value string) string {
	return sensitiveExportRE.ReplaceAllString(value, `${1}${2}[REDACTED]`)
}
