package transport

import (
	"os"
	"testing"

	"archivecheck/internal/fault"
)

func TestPlanTracksBandwidth(t *testing.T) {
	samples := []BandwidthSample{
		{At: 0, Bps: 2_000_000},
		{At: 10, Bps: 400_000},
		{At: 20, Bps: 4_000_000},
	}
	cfg := DefaultBudget(t.TempDir())
	cfg.MaxChunkBytes = 1 << 20
	plan := Plan(samples, cfg)
	if len(plan) != 3 {
		t.Fatalf("plan=%d", len(plan))
	}
	// 2 Mbps * 0.25s / 8 = 62.5 KiB; 400 kbps -> 12.5 KiB; 4 Mbps -> 125 KiB.
	if plan[0].Bytes != 62_500 || plan[1].Bytes != 12_500 || plan[2].Bytes != 125_000 {
		t.Fatalf("sizes: %d %d %d", plan[0].Bytes, plan[1].Bytes, plan[2].Bytes)
	}
	if plan[1].Bytes >= plan[2].Bytes {
		t.Fatal("slow link must yield smaller chunks")
	}
	// Determinism.
	again := Plan(samples, cfg)
	for i := range plan {
		if plan[i] != again[i] {
			t.Fatalf("nondeterministic plan at %d", i)
		}
	}
}

func TestPlanClamps(t *testing.T) {
	cfg := DefaultBudget("")
	cfg.MinChunkBytes = 1000
	cfg.MaxChunkBytes = 5000
	plan := Plan([]BandwidthSample{{At: 0, Bps: 1}, {At: 1, Bps: 1e12}}, cfg)
	if plan[0].Bytes != 1000 || plan[1].Bytes != 5000 {
		t.Fatalf("clamp: %d %d", plan[0].Bytes, plan[1].Bytes)
	}
}

func TestBufferInMemoryMatchesSpill(t *testing.T) {
	chunks := [][]byte{[]byte("alpha"), []byte("bravo"), []byte("charlie"), []byte("delta")}

	mem := NewBuffer(1024, "", &fault.Set{})
	for _, c := range chunks {
		if err := mem.Append(c); err != nil {
			t.Fatal(err)
		}
	}
	memStats := mem.Stats()
	if memStats.Spilled != 0 || memStats.PeakMemory > 1024 {
		t.Fatalf("memory stats: %+v", memStats)
	}

	dir := t.TempDir()
	spill := NewBuffer(8, dir, &fault.Set{})
	for _, c := range chunks {
		if err := spill.Append(c); err != nil {
			t.Fatal(err)
		}
	}
	spillStats := spill.Stats()
	defer spill.Close()
	if spillStats.Spilled == 0 {
		t.Fatal("expected spills under an 8-byte budget")
	}
	if spillStats.PeakMemory > 8 {
		t.Fatalf("peak %d exceeds budget", spillStats.PeakMemory)
	}
	// Determinism: identical digest for identical input regardless of spill.
	if memStats.Digest != spillStats.Digest {
		t.Fatalf("digest mismatch mem=%s spill=%s", memStats.Digest, spillStats.Digest)
	}
	if memStats.Digest != DigestChunks(chunks) {
		t.Fatalf("digest != reference")
	}
	got, err := spill.Buffered()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "alphabravocharliedelta" {
		t.Fatalf("buffered=%q", got)
	}
}

func TestBufferRejectsWithoutSpillDir(t *testing.T) {
	fs := &fault.Set{}
	b := NewBuffer(4, "", fs)
	if err := b.Append([]byte("too big")); err == nil {
		t.Fatal("expected rejection")
	}
	if fs.Count(fault.KindBudgetExceeded) != 1 {
		t.Fatalf("faults=%v", fs.Faults())
	}
}

func TestSpillFileRemoved(t *testing.T) {
	dir := t.TempDir()
	b := NewBuffer(2, dir, &fault.Set{})
	if err := b.Append([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if err := b.Append([]byte("abcdefghij")); err != nil {
		t.Fatal(err)
	}
	name := b.spill.Name()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("spill file not removed: %v", err)
	}
}

func TestBufferMemoryDoesNotGrowWithInput(t *testing.T) {
	dir := t.TempDir()
	const budget = 4096
	b := NewBuffer(budget, dir, &fault.Set{})
	defer b.Close()
	chunk := make([]byte, 1000)
	for i := 0; i < 500; i++ {
		if err := b.Append(chunk); err != nil {
			t.Fatal(err)
		}
		if b.inMem > budget {
			t.Fatalf("in-memory %d exceeded budget %d at iteration %d", b.inMem, budget, i)
		}
	}
	st := b.Stats()
	if st.Chunks != 500 {
		t.Fatalf("chunks=%d", st.Chunks)
	}
	if st.PeakMemory > budget {
		t.Fatalf("peak=%d", st.PeakMemory)
	}
}
