package main

import (
	"os"
	"path/filepath"

	"mediapack/internal/checkpoint"
)

func writeGarbageCLI(m *checkpoint.Manager, s string) error {
	return os.WriteFile(filepath.Join(m.Dir(), "checkpoint.json"), []byte(s), 0o644)
}
