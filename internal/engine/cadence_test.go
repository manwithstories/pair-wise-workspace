package engine

import (
	"testing"

	"avrepair/internal/clock"
	"avrepair/internal/fixture"
	"avrepair/internal/subtitle"
)

// TestSnapToCadenceRepairsRollback proves a bounded rollback is returned to its
// nominal slot while strict monotonicity and the 500ms budget are respected.
func TestSnapToCadenceRepairsRollback(t *testing.T) {
	sc, err := fixture.Build(fixture.CaseRollback)
	if err != nil {
		t.Fatal(err)
	}
	step := clock.Second / sc.Spec.VideoFPS
	pts := toPoints("video", sc.Video)
	rep := clock.RepairTimeline(pts)
	repaired := applyRepair(sc.Video, rep)
	snapped := snapToCadence(repaired, step)

	if err := clock.CheckMonotonic("video", collectTS(snapped)); err != nil {
		t.Fatalf("snapped stream not monotonic: %v", err)
	}
	for _, p := range snapped {
		ideal := int64(p.Seq) * step
		if abs64(p.TS-ideal) > 1 {
			t.Fatalf("seq %d off cadence: ts=%d ideal=%d", p.Seq, p.TS, ideal)
		}
	}
}

// TestSnapRespectsBudgetAndMonotonicity ensures oversized drift is left alone
// rather than forced onto a slot.
func TestSnapRespectsBudgetAndMonotonicity(t *testing.T) {
	step := int64(100)
	pkts := []fixture.Packet{
		{Seq: 0, TS: 0},
		{Seq: 1, TS: 100},
		{Seq: 2, TS: 205}, // 5ns drift, snappable
		{Seq: 3, TS: 300},
	}
	out := snapToCadence(pkts, step)
	if out[2].TS != 200 {
		t.Fatalf("small drift should snap, got %d", out[2].TS)
	}
	// A drift beyond the 500ms budget must remain untouched.
	big := []fixture.Packet{{Seq: 0, TS: 0}, {Seq: 1, TS: 100 + clock.MaxRepairDelta + 10}}
	out2 := snapToCadence(big, step)
	if out2[1].TS != 100+clock.MaxRepairDelta+10 {
		t.Fatal("over-budget drift must not be snapped")
	}
}

func TestSnapCues(t *testing.T) {
	cues := []subtitle.Cue{
		{Index: 0, StartNS: 0, EndNS: 900_000_000},
		{Index: 1, StartNS: clock.Second + 30_000_000, EndNS: 2 * clock.Second},
		{Index: 2, StartNS: 2 * clock.Second, EndNS: 3 * clock.Second},
	}
	out := snapCues(cues, clock.Second)
	for i, c := range out {
		if c.StartNS != int64(i)*clock.Second {
			t.Fatalf("cue %d not on cadence: %d", i, c.StartNS)
		}
	}
}

func collectTS(pkts []fixture.Packet) []int64 {
	out := make([]int64, len(pkts))
	for i, p := range pkts {
		out[i] = p.TS
	}
	return out
}
