// Package ingest parses the supported MXF (KLV) and MP4 (box) subsets
// into track packets carrying byte offsets into their sources.
package ingest

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SafeJoin joins name onto base and rejects any result escaping base.
func SafeJoin(base, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("absolute path rejected: %q", name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes base directory: %q", name)
	}
	return filepath.Join(base, clean), nil
}
