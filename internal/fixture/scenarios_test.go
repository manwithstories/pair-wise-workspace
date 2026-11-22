package fixture

import (
	"testing"

	"mediapack/internal/clock"
)

func TestValidFixtureParses(t *testing.T) {
	f := Valid()
	clusters, blocks, err := parseStats(t, f.Data)
	if err != nil {
		t.Fatalf("valid fixture must parse cleanly: %v", err)
	}
	if clusters != f.ClusterCount || blocks != f.BlockCount {
		t.Fatalf("counts mismatch: got c=%d b=%d want c=%d b=%d",
			clusters, blocks, f.ClusterCount, f.BlockCount)
	}
}

func TestAllHealthyFixturesParse(t *testing.T) {
	for name, f := range All() {
		if f.Truncated {
			continue // validated separately
		}
		clusters, blocks, err := parseStats(t, f.Data)
		if err != nil {
			t.Errorf("%s: unexpected parse error: %v", name, err)
			continue
		}
		if clusters != f.ClusterCount || blocks != f.BlockCount {
			t.Errorf("%s: got c=%d b=%d want c=%d b=%d",
				name, clusters, blocks, f.ClusterCount, f.BlockCount)
		}
	}
}

func TestTruncatedFixtureReportsPreciseOffset(t *testing.T) {
	f := UnknownLengthTruncated()
	_, _, err := parseStats(t, f.Data)
	me, ok := clock.AsMediaError(err)
	if !ok {
		t.Fatalf("truncated fixture must produce MediaError, got %v", err)
	}
	if me.Category != clock.CatTruncated {
		t.Fatalf("want CatTruncated, got %s", me.Category)
	}
	// The cut removed the last 5 bytes; the first missing logical byte
	// is the declared end of the final block, within 1 of physical EOF.
	physicalEOF := int64(len(f.Data))
	diff := me.Offset - physicalEOF
	if diff < 0 {
		diff = -diff
	}
	if diff > 1 {
		t.Fatalf("truncation anchor %d is %d bytes from EOF %d", me.Offset, diff, physicalEOF)
	}
}

func TestDeterminism(t *testing.T) {
	a := Valid().Data
	b := Valid().Data
	if len(a) != len(b) {
		t.Fatal("fixture length nondeterministic")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("fixture bytes differ at %d", i)
		}
	}
}

func TestTimestampWrapFixtureStructure(t *testing.T) {
	f := TimestampWrap()
	if !f.Wrap {
		t.Fatal("wrap flag missing")
	}
	if _, _, err := parseStats(t, f.Data); err != nil {
		t.Fatalf("wrap fixture must still parse structurally: %v", err)
	}
}

func TestLateAndSlowFlags(t *testing.T) {
	if !LateEvents().Late || !MultiTrackOutOfOrder().Late {
		t.Fatal("late flags not set")
	}
	if SlowConsumer().BlockCount < 50 {
		t.Fatal("slow-consumer fixture needs enough blocks to fill a queue")
	}
}
