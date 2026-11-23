package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// prepareWorkDir creates a scratch tree. A caller-supplied directory is used as
// is; otherwise a temp directory is created and removed on exit.
func prepareWorkDir(dir string) (string, func(), error) {
	if dir != "" {
		clean := filepath.Clean(dir)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return "", func() {}, fmt.Errorf("work directory escapes the working tree")
		}
		if err := os.MkdirAll(clean, 0o755); err != nil {
			return "", func() {}, err
		}
		return clean, func() {}, nil
	}
	tmp, err := os.MkdirTemp("", "mediasync-demo-*")
	if err != nil {
		return "", func() {}, err
	}
	return tmp, func() { os.RemoveAll(tmp) }, nil
}

// sanitize strips newlines so terminal output stays one key=value per line.
func sanitize(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}
