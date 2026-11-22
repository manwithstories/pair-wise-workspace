package integration

import (
	"os"
	"runtime"
	"testing"

	"mediacheck/internal/fixture"
	"mediacheck/internal/probe"
	"mediacheck/internal/repair"
)

// TestLargeSparseFileStaysBounded grows a valid fixture past 1 GiB by
// extending mdat, which a real recording can also do. The analysis must keep
// peak heap well below the file size.
func TestLargeSparseFileStaysBounded(t *testing.T) {
	dir := t.TempDir()
	gen, err := fixture.Generate(dir, fixture.DefaultSpec(fixture.Valid))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(gen.Path)
	if err != nil {
		t.Fatal(err)
	}
	// Locate mdat and inflate its declared size while appending a sparse hole
	// so the file size grows without allocating the hole on disk.
	mdat := findMdat(raw)
	if mdat < 0 {
		t.Fatal("no mdat")
	}
	const grow = int64(1) << 30
	if err := os.Truncate(gen.Path, int64(len(raw))+grow); err != nil {
		t.Fatal(err)
	}
	// Rewrite the mdat size field to cover the grown payload.
	newSize := uint32(len(raw) - mdat - 8 + int(grow))
	patch := []byte{byte(newSize >> 24), byte(newSize >> 16), byte(newSize >> 8), byte(newSize)}
	f, err := os.OpenFile(gen.Path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(patch, int64(mdat)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	rcfg := repair.DefaultConfig()
	rcfg.Recover = true
	pf, res, err := repair.New(rcfg, probe.DefaultConfig()).Analyze(gen.Path)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if pf.Size < 1<<30 {
		t.Fatalf("file is only %d bytes", pf.Size)
	}
	if len(pf.Tracks) != 2 {
		t.Fatalf("tracks=%d want 2", len(pf.Tracks))
	}
	if res.Status == repair.ResultFailed {
		t.Fatalf("large file failed: %v", res.Issues)
	}
	var peak int64
	if after.HeapAlloc > before.HeapAlloc {
		peak = int64(after.HeapAlloc - before.HeapAlloc)
	}
	// Well under the 64 MiB moov cap plus a few MiB of tables.
	if peak > 96<<20 {
		t.Fatalf("heap grew by %d bytes, expected bounded usage", peak)
	}
}

func findMdat(raw []byte) int {
	var pos int
	for pos+8 <= len(raw) {
		size := int(uint32(raw[pos])<<24 | uint32(raw[pos+1])<<16 | uint32(raw[pos+2])<<8 | uint32(raw[pos+3]))
		typ := string(raw[pos+4 : pos+8])
		if typ == "mdat" {
			return pos
		}
		if size <= 0 {
			return -1
		}
		pos += size
	}
	return -1
}
