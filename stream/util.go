package stream

import "path/filepath"

// filepathDir is a small wrapper kept separate so the runner stays focused
// on pipeline logic.
func filepathDir(p string) string { return filepath.Dir(p) }
