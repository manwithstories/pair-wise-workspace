package fixture

import (
	"bytes"
	"testing"

	"avcheck/internal/clock"
	"avcheck/internal/wav"
)

func testConfig() Config {
	c := DefaultConfig()
	c.DurationMS = 500
	c.Tracks = 2
	c.LossBurst = 3
	c.Reorder = 4
	c.Duplicate = 3
	return c
}

func TestNormalFixtureIsCompleteAndOrdered(t *testing.T) {
	g := NewGenerator(testConfig())
	g.BuildTracks()
	pkts := g.Normal()
	if len(pkts) == 0 {
		t.Fatal("no packets generated")
	}
	perTrack := map[uint32]int{}
	lastSeq := map[uint32]uint16{}
	for _, p := range pkts {
		if perTrack[p.TrackID] > 0 {
			if p.Seq != lastSeq[p.TrackID]+1 {
				t.Fatalf("track %d sequence discontinuity at %d", p.TrackID, p.Seq)
			}
		}
		lastSeq[p.TrackID] = p.Seq
		perTrack[p.TrackID]++
	}
	if len(perTrack) != 2 {
		t.Fatalf("tracks=%d, want 2", len(perTrack))
	}
}

func TestReorderAndDuplicate(t *testing.T) {
	g := NewGenerator(testConfig())
	g.BuildTracks()
	base := g.Normal()
	rd := g.ReorderAndDuplicate()
	if len(rd) <= len(base) {
		t.Fatalf("duplicates did not grow packet count: %d <= %d", len(rd), len(base))
	}
	// A duplicate sequence number must appear somewhere.
	seen := map[[2]uint32]int{}
	for _, p := range rd {
		key := [2]uint32{p.TrackID, uint32(p.Seq)}
		seen[key]++
	}
	hasDup := false
	for _, n := range seen {
		if n > 1 {
			hasDup = true
		}
	}
	if !hasDup {
		t.Fatal("no duplicated sequence detected")
	}
}

func TestLossBurstRemovesConsecutiveRuns(t *testing.T) {
	cfg := testConfig()
	g := NewGenerator(cfg)
	g.BuildTracks()
	pkts, dropped := g.LossBurst()
	if len(dropped) == 0 {
		t.Fatal("no drops recorded")
	}
	for track, seqs := range dropped {
		if len(seqs) != cfg.LossBurst {
			t.Fatalf("track %d drop count=%d, want %d", track, len(seqs), cfg.LossBurst)
		}
		for i := 1; i < len(seqs); i++ {
			if seqs[i] != seqs[i-1]+1 {
				t.Fatalf("drop %v not consecutive", seqs)
			}
		}
	}
	got := map[[2]uint32]bool{}
	for _, p := range pkts {
		got[[2]uint32{p.TrackID, uint32(p.Seq)}] = true
	}
	for track, seqs := range dropped {
		for _, s := range seqs {
			if got[[2]uint32{track, uint32(s)}] {
				t.Fatalf("dropped seq %d still present on track %d", s, track)
			}
		}
	}
}

func TestLateOverflowDelaysBeyondWindow(t *testing.T) {
	g := NewGenerator(testConfig())
	g.BuildTracks()
	const window = 4
	pkts := g.LateOverflow(window)
	if len(pkts) == 0 {
		t.Fatal("no packets")
	}
	// The final packets are the deliberately late ones.
	late := pkts[len(pkts)-g.cfg.Tracks:]
	for _, p := range late {
		if p.ArrivalMS <= 0 {
			t.Fatal("late packet has no delayed arrival")
		}
	}
}

func TestCorruptWAVReportsOffset(t *testing.T) {
	raw, offset := CorruptWAV()
	if _, err := wav.Decode(bytes.NewReader(raw)); err == nil {
		t.Fatal("truncated WAV must fail decoding")
	} else if ce, ok := err.(*clock.Error); !ok || ce.Kind != clock.KindTruncatedWAV {
		t.Fatalf("err=%v, want truncated_wav", err)
	}
	if offset <= 0 || offset >= int64(len(raw))+20 {
		t.Fatalf("offset %d outside plausible range", offset)
	}
}

func TestDeterministicAcrossGenerators(t *testing.T) {
	cfg := testConfig()
	a := NewGenerator(cfg)
	a.BuildTracks()
	b := NewGenerator(cfg)
	b.BuildTracks()
	pa, pb := a.ReorderAndDuplicate(), b.ReorderAndDuplicate()
	if len(pa) != len(pb) {
		t.Fatalf("nondeterministic lengths %d vs %d", len(pa), len(pb))
	}
	for i := range pa {
		if !bytes.Equal(pa[i].Raw, pb[i].Raw) {
			t.Fatalf("packet %d differs across seeded runs", i)
		}
	}
}
