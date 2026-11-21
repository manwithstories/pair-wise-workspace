package repair

import (
	"math"
	"testing"

	"mediad/internal/clock"
	"mediad/internal/model"
)

func at(n, d int64) clock.Rational {
	r, err := clock.New(n, d)
	if err != nil {
		panic(err)
	}
	return r
}

func makeSamples(ticks []int64, den int64) []model.Sample {
	out := make([]model.Sample, len(ticks))
	for i, t := range ticks {
		r := at(t, den)
		out[i] = model.Sample{Index: i, DTS: r, PTS: r, Duration: at(1, den)}
	}
	return out
}

func TestDuplicatesAreResolved(t *testing.T) {
	samples := makeSamples([]int64{0, 3000, 3000, 3000, 9000}, 90000)
	out, rep, err := Timestamps(1, samples, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Duplicates != 2 || rep.Adjusted != 2 {
		t.Fatalf("report=%+v", rep)
	}
	for i := 1; i < len(out); i++ {
		if !out[i-1].DTS.Less(out[i].DTS) {
			t.Fatalf("DTS not increasing at %d: %v then %v", i, out[i-1].DTS, out[i].DTS)
		}
	}
	// The untouched samples keep their exact timestamps.
	if out[1].DTS != at(3000, 90000) || out[4].DTS != at(9000, 90000) {
		t.Fatalf("repair disturbed intact samples: %v %v", out[1].DTS, out[4].DTS)
	}
	// The input is not modified.
	if samples[2].DTS != at(3000, 90000) {
		t.Fatal("input slice was modified")
	}
}

func TestWrapIsUnwrappedAndReported(t *testing.T) {
	// 90 kHz timestamps crossing the 32-bit boundary.
	const mod = 1 << 32
	start := int64(mod - 3*3000)
	ticks := []int64{start, start + 3000, start + 6000, start + 9000}
	// Express the post-wrap values as they arrive on the wire.
	samples := makeSamples([]int64{
		ticks[0], ticks[1], ticks[2] - mod, ticks[3] - mod,
	}, 90000)
	out, rep, err := Timestamps(1, samples, Options{WrapTicks: mod, ClockRate: 90000})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Wraps != 2 {
		t.Fatalf("wraps=%d want 2", rep.Wraps)
	}
	for i := 1; i < len(out); i++ {
		if !out[i-1].DTS.Less(out[i].DTS) {
			t.Fatalf("wrap not repaired at %d", i)
		}
		if d, _ := out[i].DTS.Sub(out[i-1].DTS); d != at(3000, 90000) {
			t.Fatalf("spacing at %d is %v", i, d)
		}
	}
}

func TestDriftIsMeasuredAndBounded(t *testing.T) {
	// 30 frames at exactly 1/30 s, then a 0.1% slow clock.
	const n = 31
	ticks := make([]int64, n)
	step := int64(1_000_000 / 30) // microseconds per frame
	for i := range ticks {
		ticks[i] = int64(i) * step
	}
	samples := makeSamples(ticks, 1_000_000)
	_, rep, err := Timestamps(1, samples, Options{NominalFrameRate: at(30, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(rep.DriftPPM) > 1000 {
		t.Fatalf("exact stream reported drift %.1f ppm", rep.DriftPPM)
	}

	slow := make([]model.Sample, n)
	for i := range slow {
		// Each frame is 1001/30000 s instead of 1/30 s: a uniform clock
		// that runs 0.1% slow.
		r := at(int64(i)*1001, 30000)
		slow[i] = model.Sample{Index: i, DTS: r, PTS: r}
	}
	_, rep2, err := Timestamps(1, slow, Options{NominalFrameRate: at(30, 1)})
	if err != nil {
		t.Fatal(err)
	}
	// 1001/30000 vs 1/30 is +1000 ppm: the media clock is 0.1% long.
	if math.Abs(rep2.DriftPPM-1000) > 50 {
		t.Fatalf("drift=%.1f ppm want about +1000", rep2.DriftPPM)
	}

	_, _, err = Timestamps(1, slow, Options{NominalFrameRate: at(30, 1), MaxDriftPPM: 100})
	if err == nil {
		t.Fatal("expected drift limit error")
	}
	if model.KindOf(err) != model.KindInvalidMedia {
		t.Fatalf("kind=%v", model.KindOf(err))
	}
}

func TestPTSOffsetPreserved(t *testing.T) {
	samples := makeSamples([]int64{0, 3000, 3000}, 90000)
	samples[1].PTS = at(3300, 90000)
	samples[2].PTS = at(3600, 90000)
	out, _, err := Timestamps(1, samples, Options{})
	if err != nil {
		t.Fatal(err)
	}
	off, err := out[1].PTS.Sub(out[1].DTS)
	if err != nil {
		t.Fatal(err)
	}
	if off != at(300, 90000) {
		t.Fatalf("PTS offset=%v want 300/90000", off)
	}
}

func TestReportOffsets(t *testing.T) {
	samples := makeSamples([]int64{100, 200, 300}, 1000)
	_, rep, err := Timestamps(4, samples, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.BeforeOffset != at(100, 1000) || rep.AfterOffset != at(300, 1000) {
		t.Fatalf("offsets=%v..%v", rep.BeforeOffset, rep.AfterOffset)
	}
	if rep.AppliedOffset != at(200, 1000) {
		t.Fatalf("applied=%v", rep.AppliedOffset)
	}
	if rep.TrackID != 4 {
		t.Fatalf("track=%d", rep.TrackID)
	}
}

func TestEmptyInput(t *testing.T) {
	if _, _, err := Timestamps(1, nil, Options{}); err == nil {
		t.Fatal("expected error for empty samples")
	}
}

func TestTrackRepairsDuration(t *testing.T) {
	tr := model.Track{ID: 2, Kind: model.Video, CodedWidth: 64, CodedHeight: 64,
		FrameRate: at(30, 1), Samples: makeSamples([]int64{0, 3000, 3000}, 90000)}
	if _, err := Track(&tr, Options{}); err != nil {
		t.Fatal(err)
	}
	if !tr.Samples[2].DTS.Less(clock.FromInt(1)) {
		t.Fatalf("last DTS %v", tr.Samples[2].DTS)
	}
	for i := 1; i < len(tr.Samples); i++ {
		if !tr.Samples[i-1].DTS.Less(tr.Samples[i].DTS) {
			t.Fatalf("not monotonic at %d", i)
		}
	}
}
