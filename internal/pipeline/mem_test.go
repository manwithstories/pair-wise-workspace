package pipeline

import (
	"os"
	"runtime"
	"testing"

	"mediascan/internal/fixture"
)

// TestMemoryBudget models the documented worst case: 12 segments padded to the
// 16 MiB per-segment cap. Analysis streams, so heap in use after the run must
// stay far below the 128 MiB resident target.
func TestMemoryBudget(t *testing.T) {
	work := t.TempDir()
	l, err := fixture.Generate(work)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < fixture.SegmentCount; i++ {
		if i == l.MissingIdx {
			continue
		}
		path := fixture.SegmentPath(work, i)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, 16<<20); err != nil {
			t.Fatal(err)
		}
		// Zero padding keeps the box structure valid at the front; the trailing
		// zero bytes are not a valid box header, which the scanner accounts for
		// when hashing (hashing does not parse) and analysis tolerates.
		if info.Size() > 16<<20 {
			t.Fatalf("segment already over cap")
		}
	}
	if _, err := fixture.GenerateRTP(l.RTPFile, 240, 17); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	o := Options{Work: work, LossEvery: 17, RTPPackets: 240}
	if _, err := Run(o, 0); err != nil {
		t.Fatal(err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.HeapInuse > 128<<20 {
		t.Fatalf("heap in use %d MiB exceeds 128 MiB budget", after.HeapInuse>>20)
	}
	t.Logf("heap_inuse=%dMiB", after.HeapInuse>>20)
}
