package repair

import (
	"testing"

	"mediarchive/internal/clock"
	"mediarchive/internal/event"
	"mediarchive/internal/index"
)

func seq(start, n uint64, step clock.Time) []*event.Packet {
	out := make([]*event.Packet, 0, n)
	for i := uint64(0); i < n; i++ {
		out = append(out, &event.Packet{
			SeqExt: start + i, TSExt: (start + i) * 960,
			MediaTime: clock.Time(start+i) * step, Duration: step,
			Payload: []byte{1, 2, 3},
		})
	}
	return out
}

func TestLossBurstGapAnnotated(t *testing.T) {
	pkts := append(seq(100, 5, 1800), seq(108, 3, 1800)...)
	res := Repair(pkts, nil)
	if len(res.Gaps) != 1 {
		t.Fatalf("gaps=%d want 1", len(res.Gaps))
	}
	g := res.Gaps[0]
	if g.AfterSeq != 104 || g.Count != 3 || g.Source != "loss_burst" || g.Confidence != 1.0 {
		t.Fatalf("gap=%+v", g)
	}
	if g.Start != 105*1800 || g.End != 108*1800 {
		t.Fatalf("gap interval [%d,%d)", g.Start, g.End)
	}
	if res.Fatal() {
		t.Fatal("bounded loss must be recoverable")
	}
}

func TestRegressiveTimestampRepaired(t *testing.T) {
	pkts := seq(0, 4, 1800)
	pkts[2].MediaTime = 100 // regressive
	res := Repair(pkts, nil)
	if res.RepairedTS != 1 {
		t.Fatalf("repaired=%d want 1", res.RepairedTS)
	}
	if pkts[2].MediaTime != 2*1800 || pkts[2].Flags&event.FlagRepairedTS == 0 {
		t.Fatalf("ts=%d flags=%v", pkts[2].MediaTime, pkts[2].Flags)
	}
	// Durations of neighbours must not be double-counted after repair.
	for i := 1; i < len(pkts); i++ {
		if pkts[i].MediaTime != pkts[i-1].MediaTime+pkts[i-1].Duration {
			t.Fatalf("packet %d not contiguous after repair", i)
		}
	}
}

func TestWrapAmbiguityFails(t *testing.T) {
	pkts := append(seq(0, 2, 1800), seq(0x9000, 2, 1800)...)
	res := Repair(pkts, nil)
	if !res.Fatal() {
		t.Fatal("gap beyond half cycle must be fatal")
	}
	found := false
	for _, d := range res.Degraded {
		if d.Reason == event.ReasonWrapAmbiguous {
			found = true
		}
	}
	if !found {
		t.Fatalf("degraded=%+v", res.Degraded)
	}
}

func TestTailTruncationDegrades(t *testing.T) {
	pkts := seq(0, 5, 1800)
	q := []event.Quarantine{{SeqExt: 6, HasSeq: true, Reason: event.ReasonTruncated}}
	res := Repair(pkts, q)
	if len(res.Gaps) != 1 || res.Gaps[0].Source != "tail_truncation" || res.Gaps[0].Confidence != 0.5 {
		t.Fatalf("gaps=%+v", res.Gaps)
	}
	if res.Fatal() {
		t.Fatal("tail truncation must degrade, not fail")
	}
	if len(res.Degraded) == 0 {
		t.Fatal("missing degradation record")
	}
}

func TestNoBoundaryFails(t *testing.T) {
	res := Repair(nil, nil)
	if !res.Fatal() {
		t.Fatal("empty stream must be fatal")
	}
}

func TestIndexNoFabricatedEntries(t *testing.T) {
	pkts := append(seq(0, 10, 1800), seq(20, 10, 1800)...)
	res := Repair(pkts, nil)
	idx := index.Build(res.Packets, 4*1800)
	for _, e := range idx.Entries {
		// Every entry must point at a real packet.
		found := false
		for _, p := range res.Packets {
			if p.SeqExt == e.SeqExt && p.MediaTime == e.Time {
				found = true
			}
		}
		if !found {
			t.Fatalf("fabricated entry %+v", e)
		}
	}
	// Find returns the latest entry at or before t.
	e, ok := idx.Find(5 * 1800)
	if !ok || e.Time > 5*1800 {
		t.Fatalf("find=%+v ok=%v", e, ok)
	}
	if _, ok := idx.Find(-1); ok {
		t.Fatal("find before first entry must fail")
	}
	// Truncated packets are never access points.
	res.Packets[3].Flags |= event.FlagTruncated
	idx2 := index.Build(res.Packets, 3*1800)
	for _, e := range idx2.Entries {
		if e.SeqExt == res.Packets[3].SeqExt {
			t.Fatal("truncated packet used as access point")
		}
	}
}
