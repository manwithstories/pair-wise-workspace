package timeline

import (
	"testing"

	"github.com/clearliu/fieldcut/internal/clock"
	"github.com/clearliu/fieldcut/internal/ingest"
)

func frame(track, seq int, ts uint32) *Event {
	return &Event{TrackID: track, Sequence: seq, Kind: KindFrame, RawTS: ts, Source: ingest.SourceLive}
}

func TestTrackRepairWrapAndMonotonic(t *testing.T) {
	rep := NewTrackRepair(DefaultRepairConfig())
	// RTP 32-bit wrap near the modulus: last value sits one frame above
	// the modulus so the extended axis demonstrably crossed 2^32.
	base := uint32(clock.Max32 - 9000)
	steps := []uint32{base, base + 9000, base + 18000, 7, 18007}
	var prev clock.Ticks
	for i, ts := range steps {
		fixed, untrusted, _, err := rep.Repair(ts)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && fixed < prev {
			t.Fatalf("step %d ts=%d went backwards: %d < %d", i, ts, fixed, prev)
		}
		if untrusted {
			t.Fatalf("step %d unexpectedly untrusted", i)
		}
		prev = fixed
	}
	// After wrap the fixed axis must have crossed the 32-bit modulus:
	// the penultimate raw 7 is unwrapped to Max32+7 (then clamped to the
	// prior frame because it is a tiny non-winding rewind), and the final
	// raw 18007 lands at Max32+18007.
	if prev < clock.Ticks(clock.Max32) {
		t.Fatalf("wrapped value did not cross modulus: %d", prev)
	}
	if got := prev - clock.Ticks(clock.Max32); got != 18007 {
		t.Fatalf("post-wrap offset = %d, want 18007", got)
	}
}

func TestTrackRepairRegressionAndDuplicate(t *testing.T) {
	rep := NewTrackRepair(DefaultRepairConfig())
	fix := func(ts uint32) (clock.Ticks, bool, string) {
		f, u, d, err := rep.Repair(ts)
		if err != nil {
			t.Fatal(err)
		}
		return f, u, d
	}
	a, _, _ := fix(100000)
	b, _, d := fix(100900)
	if b <= a || d != "" {
		t.Fatalf("normal advance wrong: %d %d %q", a, b, d)
	}
	// Small backward jump: repaired by clamping, defect recorded.
	c, untrusted, d := fix(100500)
	if untrusted || d != "regression" || c != b {
		t.Fatalf("regression repair wrong: c=%d b=%d untrusted=%v d=%q", c, b, untrusted, d)
	}
	// Duplicate wire timestamp.
	_, _, d = fix(100500)
	if d != "duplicate" {
		t.Fatalf("duplicate defect = %q", d)
	}
	// Large backward jump: cannot be uniquely inferred -> untrusted.
	_, untrusted, d = fix(1000)
	if !untrusted || d == "" {
		t.Fatalf("large regression must be untrusted: untrusted=%v d=%q", untrusted, d)
	}
}

func TestTrackRepairDriftAnchor(t *testing.T) {
	rep := NewTrackRepair(DefaultRepairConfig())
	// Local clock runs 0.1% fast against the reference.
	for i := 0; i < 4; i++ {
		local := clock.Ticks(i * 90000)
		ref := clock.Ticks(float64(local) * 0.999)
		if err := rep.Anchor(local, ref); err != nil {
			t.Fatal(err)
		}
	}
	if !rep.haveFit {
		t.Fatal("drift model should be fitted after 4 anchors")
	}
	if rep.slope >= 1.0 || rep.slope < 0.998 {
		t.Fatalf("slope %.6f outside expected drift envelope", rep.slope)
	}
}

func TestWatermarkCrossTrackCommit(t *testing.T) {
	w := NewWatermark(DefaultRepairConfig())
	commit := func(ev *Event) int {
		out, err := w.Commit(ev)
		if err != nil {
			t.Fatal(err)
		}
		return len(out)
	}
	// Two tracks at 90kHz. Both t=0 frames cross mark 0 immediately.
	// Video then races to t=90000 while audio stays at 0, so the fast
	// track's future frame must be held back by the slow track.
	if n := commit(frame(0, 0, 0)); n != 1 {
		t.Fatalf("video first frame: %d", n)
	}
	if n := commit(frame(1, 0, 0)); n != 1 {
		t.Fatalf("audio first frame: %d", n)
	}
	if n := commit(frame(0, 1, 90000)); n != 0 {
		t.Fatalf("video ahead of audio must be held: %d", n)
	}
	// Audio catches up: mark reaches 90000 and the held video frame plus
	// this audio frame are released together in total order.
	out, err := w.Commit(frame(1, 1, 90000))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("released %d events at mark 90000, want 2", len(out))
	}
	if out[0].TrackID != 0 || out[1].TrackID != 1 || out[0].Start != out[1].Start {
		t.Fatalf("total order wrong: %+v %+v", out[0], out[1])
	}
}

func TestWatermarkGapPassesThrough(t *testing.T) {
	w := NewWatermark(DefaultRepairConfig())
	gap := &Event{TrackID: 0, Sequence: 5, Kind: KindGap, Start: 500000}
	out, err := w.Commit(gap)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !out[0].Untrusted || out[0].Kind != KindGap {
		t.Fatalf("gap handling wrong: %+v", out)
	}
}

func TestWatermarkFlushAndDeterminism(t *testing.T) {
	build := func() []*Event {
		w := NewWatermark(DefaultRepairConfig())
		// Both tracks established first, then video frames arrive out of
		// order and audio trails so the watermark actually buffers them.
		events := []*Event{
			frame(0, 0, 0), frame(1, 0, 0),
			frame(0, 2, 180000), frame(0, 1, 90000),
			frame(1, 1, 90000), frame(1, 2, 180000),
		}
		var all []*Event
		for _, ev := range events {
			out, err := w.Commit(ev)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, out...)
		}
		all = append(all, w.Flush()...)
		return all
	}
	first := build()
	second := build()
	if len(first) != len(second) {
		t.Fatalf("run lengths differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Start != second[i].Start ||
			first[i].TrackID != second[i].TrackID ||
			first[i].Sequence != second[i].Sequence {
			t.Fatalf("non-deterministic order at %d: %+v vs %+v", i, first[i], second[i])
		}
	}
	// The committed prefix and Flush output must each be non-decreasing;
	// Flush can restart from a lower tick when drained queues differ, so
	// the check is applied within the two segments rather than globally.
	for i := 1; i < 4; i++ {
		if first[i].Start < first[i-1].Start {
			t.Fatalf("committed prefix not sorted at %d", i)
		}
	}
	flushed := first[4:]
	for i := 1; i < len(flushed); i++ {
		if flushed[i].Start < flushed[i-1].Start {
			t.Fatalf("flush segment not sorted at %d", i)
		}
	}
}
