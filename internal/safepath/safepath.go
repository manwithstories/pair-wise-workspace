// Package safepath confines file access to a chosen root directory and
// rejects path traversal outside it.
package safepath

import (
	"fmt"
	"path/filepath"
	"strings"

	"mediarchive/internal/event"
)

// Join joins name to root and rejects any path that would escape root.
func Join(root, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", &event.Error{Op: "safepath.join", Reason: event.ReasonPathTraversal,
			Err: fmt.Errorf("absolute path %q rejected", name)}
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", &event.Error{Op: "safepath.join", Reason: event.ReasonPathTraversal,
			Err: fmt.Errorf("path %q escapes root", name)}
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", &event.Error{Op: "safepath.join", Reason: event.ReasonPathTraversal, Err: err}
	}
	full := filepath.Join(rootAbs, clean)
	rel, err := filepath.Rel(rootAbs, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", &event.Error{Op: "safepath.join", Reason: event.ReasonPathTraversal,
			Err: fmt.Errorf("path %q escapes root", name)}
	}
	return full, nil
}
