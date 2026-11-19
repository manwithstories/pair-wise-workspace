package clock

import (
	"testing"

	"archive.local/recover/internal/errs"
	"archive.local/recover/internal/model"
)

func ts(n int, step int64) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(i) * step
	}
	return out
}

func TestClockToNsExact(t *testing.T) {
	cases := []struct {
		ticks int64
		scale uint32
		want  int64
	}{
		{0, 90000, 0},
		{90000, 90000, 1_000_000_000},
		{1, 90000, 11111},
		{48000, 48000, 1_000_000_000},
		{1, 1_000_000_000, 1},
		{25, 25, 1_000_000_000},
		{-90000, 90000, -1_000_000_000},
	}
	for _, c := range cases {
		if got := model.ClockToNs(c.ticks, c.scale); got != c.want {
			t.Errorf("ClockToNs(%d,%d)=%d want %d", c.ticks, c.scale, got, c.want)
		}
	}
}

func TestUnwrapCounterRollover(t *testing.T) {
	// A 32-bit 90 kHz clock wrapping through zero. The values must be close to
	// the 2^32 boundary; otherwise the step is a genuine discontinuity, not a
	// counter rollover.
	raw := []int64{
		0xFFFFFF00, 0xFFFFFF80, 0xFFFFFFFF,
		0x00000080, 0x00000100, 0x00000180,
	}
	r, err := Unwrap(Input{TrackID: 1, Kind: model.KindVideo, Timescale: 90000, RawPTS: raw})
	if err != nil {
		t.Fatal(err)
	}
	if r.Wraps != 1 {
		t.Fatalf("wraps=%d want 1", r.Wraps)
	}
	if !monotonic(r.Track.PTS) {
		t.Fatalf("program time not monotonic: %v", r.Track.PTS)
	}
	if got, want := r.Track.PTS[3]-r.Track.PTS[2], int64(0x81*11111); got < want-1000 || got > want+1000 {
		t.Fatalf("post-wrap spacing %d ns, want about %d", got, want)
	}
	if len(r.Track.PTS) != len(raw) {
		t.Fatalf("sample count changed: %d != %d", len(r.Track.PTS), len(raw))
	}
}

func TestUnwrapRepeatsAndBackwardNoDrops(t *testing.T) {
	raw := []int64{0, 3000, 3000, 6000, 5000, 9000}
	r, err := Unwrap(Input{TrackID: 2, Kind: model.KindAudio, Timescale: 90000, RawPTS: raw})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Track.PTS) != len(raw) {
		t.Fatalf("dropped samples: %d != %d", len(r.Track.PTS), len(raw))
	}
	if r.Repeats != 1 || r.Backward != 1 {
		t.Fatalf("repeats=%d backward=%d want 1/1", r.Repeats, r.Backward)
	}
	if !monotonic(r.Track.PTS) {
		t.Fatalf("non-monotonic output: %v", r.Track.PTS)
	}
	if !r.Issues.Has(errs.CodeClockRepeat) || !r.Issues.Has(errs.CodeClockBackward) {
		t.Fatalf("classification missing: %v", r.Issues)
	}
}

func TestUnwrapVFRSamplesPreserved(t *testing.T) {
	// Variable frame rate: 25, 25, 40, 40, 20 ms gaps.
	stamps := []int64{0, 900, 2340, 3780, 4500}
	r, err := Unwrap(Input{TrackID: 1, Kind: model.KindVideo, Timescale: 90000, RawPTS: stamps})
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{0, 10e6, 26e6, 42e6, 50e6}
	for i := range want {
		if r.Track.PTS[i] != want[i] {
			t.Errorf("sample %d: got %d want %d", i, r.Track.PTS[i], want[i])
		}
	}
}

func TestDriftAnchorsCorrectMapping(t *testing.T) {
	raw := ts(10, 3000) // 30 fps at 90 kHz
	anchors := make([]Anchor, 0, 5)
	for i := 0; i < 10; i += 2 {
		// Program time advances 0.5% slower than nominal.
		anchors = append(anchors, Anchor{Sample: i, ProgramNs: int64(float64(model.ClockToNs(raw[i], 90000)) * 1.005)})
	}
	r, err := Unwrap(Input{TrackID: 1, Kind: model.KindVideo, Timescale: 90000, RawPTS: raw, Anchors: anchors})
	if err != nil {
		t.Fatal(err)
	}
	if r.Track.DriftPPM < 4000 || r.Track.DriftPPM > 6000 {
		t.Fatalf("drift ppm=%d want about 5000", r.Track.DriftPPM)
	}
	// Anchor at sample 8 should be matched within a microsecond.
	got := r.Track.PTS[8]
	want := anchors[4].ProgramNs
	if d := got - want; d < -2000 || d > 2000 {
		t.Fatalf("anchor mismatch %d ns", d)
	}
}

func TestDriftBreakSplitsAndDoesNotExtrapolate(t *testing.T) {
	raw := ts(20, 3000)
	anchors := []Anchor{
		{Sample: 0, ProgramNs: 0},
		{Sample: 4, ProgramNs: model.ClockToNs(raw[4], 90000)},
		{Sample: 8, ProgramNs: model.ClockToNs(raw[8], 90000)},
		// A clock-reset break of +500 ms at sample 12.
		{Sample: 12, ProgramNs: model.ClockToNs(raw[12], 90000) + 500_000_000},
		{Sample: 16, ProgramNs: model.ClockToNs(raw[16], 90000) + 500_000_000},
	}
	r, err := Unwrap(Input{TrackID: 1, Kind: model.KindVideo, Timescale: 90000, RawPTS: raw, Anchors: anchors})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Segments) != 2 {
		t.Fatalf("segments=%d want 2: %v", len(r.Segments), r.Segments)
	}
	if len(r.Breakpoints) != 1 || r.Breakpoints[0] != 12 {
		t.Fatalf("breakpoints=%v want [12]", r.Breakpoints)
	}
	if !r.Issues.Has(errs.CodeClockDriftBreak) {
		t.Fatalf("drift break not classified: %v", r.Issues)
	}
	// The second segment must start at its own anchor, not continue the first
	// segment's prediction.
	want := model.ClockToNs(raw[12], 90000) + 500_000_000
	if d := r.Track.PTS[12] - want; d < -2000 || d > 2000 {
		t.Fatalf("segment boundary not anchored: off by %d ns", d)
	}
}

func TestUnwrapRejectsOversizedInput(t *testing.T) {
	if _, err := Unwrap(Input{TrackID: 1, RawPTS: make([]int64, MaxSamples+1)}); err == nil {
		t.Fatal("expected size error")
	}
}

func monotonic(v []int64) bool {
	for i := 1; i < len(v); i++ {
		if v[i] < v[i-1] {
			return false
		}
	}
	return true
}

func TestUnwrapCounterNotReBASEDOnSmallBackwardStep(t *testing.T) {
	// A small backward step must be repaired, not mistaken for a rollover.
	raw := []int64{100_000, 200_000, 150_000, 300_000}
	r, err := Unwrap(Input{TrackID: 1, Kind: model.KindVideo, Timescale: 90000, RawPTS: raw})
	if err != nil {
		t.Fatal(err)
	}
	if r.Wraps != 0 {
		t.Fatalf("small backward step treated as wrap: %d", r.Wraps)
	}
	if r.Backward != 1 {
		t.Fatalf("backward=%d want 1", r.Backward)
	}
	if !monotonic(r.Track.PTS) || len(r.Track.PTS) != len(raw) {
		t.Fatalf("repair broke timeline: %v", r.Track.PTS)
	}
}

func TestUnwrapZeroTimescaleRejected(t *testing.T) {
	// A zero timescale would map every sample to 0; instead it must be
	// surfaced by validate, not silently produce a degenerate timeline.
	r, err := Unwrap(Input{TrackID: 1, Timescale: 0, RawPTS: []int64{1, 2, 3}})
	if err != nil {
		return // rejection is acceptable
	}
	if !r.Issues.Has(errs.CodeClockRepeat) && !r.Issues.Has(errs.CodeClockBackward) {
		t.Fatalf("degenerate timeline not flagged: %v", r.Issues)
	}
}

func TestUnwrapAnchorOutOfRangeReported(t *testing.T) {
	raw := ts(10, 3000)
	r, err := Unwrap(Input{TrackID: 1, Kind: model.KindVideo, Timescale: 90000, RawPTS: raw,
		Anchors: []Anchor{{Sample: 99, ProgramNs: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Issues.Has(errs.CodeClockAnchorBad) {
		t.Fatalf("bad anchor not reported: %v", r.Issues)
	}
}
