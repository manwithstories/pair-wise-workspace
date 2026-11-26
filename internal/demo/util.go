package demo

import "os"

// mkdirAll is a thin wrapper so the orchestration file stays declarative.
func mkdirAll(dir string) error {
	return os.MkdirAll(dir, 0o755)
}
