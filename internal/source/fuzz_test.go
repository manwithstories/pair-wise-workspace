package source

import (
	"os"
	"path/filepath"
	"testing"

	"mediq/internal/model"
)

// FuzzReader feeds arbitrary bytes to the record scanner. It must always make
// progress and never panic; any outcome is either a fault or end of stream.
func FuzzReader(f *testing.F) {
	f.Add([]byte(model.RecordMagic))
	f.Add([]byte("MFRM\x01\x01\x00\x48"))
	f.Add(make([]byte, 300))
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "in")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		r, err := Open(path, Options{MaxFrameBytes: 1 << 20, MaxMetadataBytes: 1 << 16})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for i := 0; i < 100000; i++ {
			if _, err := r.Read(); err != nil {
				return
			}
		}
		t.Fatal("reader did not terminate")
	})
}
