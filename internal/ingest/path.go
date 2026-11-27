package ingest

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// SafePath rejects absolute paths, parent traversal and drive letters so a
// manifest cannot reach outside its own directory tree.
func SafePath(p string) error {
	if p == "" {
		return errors.New("empty path")
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("absolute path %q", p)
	}
	if strings.ContainsAny(p, "\\\x00") {
		return fmt.Errorf("illegal characters in %q", p)
	}
	if strings.Contains(p, ":") {
		return fmt.Errorf("path %q contains a drive or scheme", p)
	}
	raw := strings.Split(p, string(filepath.Separator))
	for _, part := range raw {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("path %q has an empty or parent segment", p)
		}
	}
	clean := filepath.Clean(p)
	if clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes its root", p)
	}
	return nil
}
