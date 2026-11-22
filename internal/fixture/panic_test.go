package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"archive/internal/demux"
)

// TestTruncatedPrefixesNeverPanic walks many prefix lengths of a real
// generated segment. Each prefix must either parse or return a fault; a
// panic would be a crash on damaged archive data, which is exactly what
// the archive job must survive.
func TestTruncatedPrefixesNeverPanic(t *testing.T) {
	root := t.TempDir()
	lay, err := Generate(root, Options{
		Segments: 1, TruncateAt: -1, MissingAt: -1, DuplicateAt: -1, ConflictAt: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, lay.MediaDir, "seg-00000.m4s"))
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n <= len(data); n += 53 {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("prefix %d panicked: %v", n, r)
				}
			}()
			_, _ = demux.Demux(data[:n], "prefix.m4s")
		}()
	}
}
