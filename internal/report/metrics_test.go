package report

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExceeds(t *testing.T) {
	ok := Summary{AVSyncMaxMS: 10, PacketRecoveryPct: 99, SeekP95MS: 5}
	if v := ok.Exceeds(DefaultThresholds); len(v) != 0 {
		t.Fatalf("violations: %v", v)
	}
	bad := Summary{AVSyncMaxMS: 30, PacketRecoveryPct: 80, SeekP95MS: 50}
	if v := bad.Exceeds(DefaultThresholds); len(v) != 3 {
		t.Fatalf("violations: %v", v)
	}
}

func TestPercentile(t *testing.T) {
	vals := []float64{5, 1, 3, 2, 4}
	if got := Percentile(vals, 50); got != 3 {
		t.Fatalf("p50=%v", got)
	}
	if got := Percentile(vals, 95); got != 5 {
		t.Fatalf("p95=%v", got)
	}
	if got := Percentile(nil, 95); got != 0 {
		t.Fatalf("empty=%v", got)
	}
}

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.bin")
	if err := AtomicWrite(p, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil || string(data) != "hello" {
		t.Fatalf("read: %v %q", err, data)
	}
	// Overwrite keeps working and leaves no temp files.
	if err := AtomicWrite(p, []byte("world")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestSHA256HexDeterministic(t *testing.T) {
	a := SHA256Hex([]byte("x"), []byte("y"))
	b := SHA256Hex([]byte("x"), []byte("y"))
	if a != b || len(a) != 64 {
		t.Fatalf("sha: %q", a)
	}
}
