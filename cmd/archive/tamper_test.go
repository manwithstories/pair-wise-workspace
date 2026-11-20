package main

import (
	"os"
	"testing"
)

// writeTamper flips a byte inside a file to simulate on-disk corruption.
func writeTamper(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read for tamper: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("cannot tamper an empty file")
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write tamper: %v", err)
	}
}
