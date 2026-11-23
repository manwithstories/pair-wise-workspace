package audio

import (
	"testing"

	"medpipe/internal/clock"
	"medpipe/internal/model"
)

func audioTimeline(t *testing.T, values []float64, stepNS int64, originNS int64) (*clock.Timeline, map[string][]model.Frame) {
	t.Helper()
	// A 1MHz tick rate keeps fixture math in whole ticks.
	const timescale = 1_000_000
	stepTicks := uint64(stepNS) / 1_000
	tr := model.Track{ID: "mic", Kind: model.KindAudio, Timescale: timescale, ClockBits: 32, NominalTickDelta: stepTicks, OriginNS: originNS}
	frames := make([]model.Frame, len(values))
	ticks := uint64(1000)
	for i, v := range values {
		frames[i] = model.Frame{TrackID: "mic", Seq: uint64(i), RawTS: ticks, Energy: []float64{v}, Payload: []byte{1}}
		ticks += stepTicks
	}
	tl, err := clock.Build([]model.Track{tr}, map[string][]model.Frame{"mic": frames}, clock.Options{NoDriftCorrection: true})
	if err != nil {
		t.Fatal(err)
	}
	return tl, map[string][]model.Frame{"mic": frames}
}

func TestDetectBasicSilence(t *testing.T) {
	// 10ms frames: loud, silent x3, loud x2, silent x2, loud.
	vals := []float64{0.8, 0.01, 0.02, 0.0, 0.9, 0.7, 0.03, 0.01, 0.8}
	tl, frames := audioTimeline(t, vals, 10_000_000, 0)
	samples, err := Collect(tl, frames)
	if err != nil {
		t.Fatal(err)
	}
	opt := Options{WindowNS: 10_000_000, Threshold: 0.05, MinSilenceNS: 10_000_000}
	ranges, err := Detect(samples, tl.StartNS, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 2 {
		t.Fatalf("ranges = %+v, want 2", ranges)
	}
	if ranges[0].StartNS != 10_000_000 || ranges[0].EndNS != 40_000_000 {
		t.Errorf("range0 = [%d,%d)", ranges[0].StartNS, ranges[0].EndNS)
	}
	if ranges[1].StartNS != 60_000_000 || ranges[1].EndNS != 80_000_000 {
		t.Errorf("range1 = [%d,%d)", ranges[1].StartNS, ranges[1].EndNS)
	}
	if TotalSilenceNS(ranges) != 50_000_000 {
		t.Errorf("total silence = %d", TotalSilenceNS(ranges))
	}
}

func TestDetectEmptyAndShortTracks(t *testing.T) {
	// Empty track: no samples, no ranges, no error.
	ranges, err := Detect(nil, 0, DefaultOptions())
	if err != nil || len(ranges) != 0 {
		t.Fatalf("empty: %v %v", ranges, err)
	}

	// 5ms track, 20ms window: one partial window still gets a verdict.
	tl, frames := audioTimeline(t, []float64{0.0}, 5_000_000, 0)
	samples, err := Collect(tl, frames)
	if err != nil {
		t.Fatal(err)
	}
	opt := Options{WindowNS: 20_000_000, Threshold: 0.05, MinSilenceNS: 1}
	ranges, err = Detect(samples, 0, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 || ranges[0].EndNS-ranges[0].StartNS != 5_000_000 {
		t.Fatalf("short track = %+v", ranges)
	}
}

func TestDetectMinSilenceDropsShortRuns(t *testing.T) {
	vals := []float64{0.9, 0.0, 0.9, 0.0, 0.0, 0.0, 0.9}
	tl, frames := audioTimeline(t, vals, 10_000_000, 0)
	samples, _ := Collect(tl, frames)
	opt := Options{WindowNS: 10_000_000, Threshold: 0.05, MinSilenceNS: 15_000_000}
	ranges, err := Detect(samples, 0, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 || ranges[0].StartNS != 30_000_000 {
		t.Fatalf("ranges = %+v", ranges)
	}
}

func TestWindowBoundarySamplesAttributedOnce(t *testing.T) {
	// 15ms frames against a 20ms window force samples to straddle boundaries.
	vals := []float64{0.0, 0.0, 0.0, 0.0}
	tl, frames := audioTimeline(t, vals, 15_000_000, 0)
	samples, err := Collect(tl, frames)
	if err != nil {
		t.Fatal(err)
	}
	opt := Options{WindowNS: 20_000_000, Threshold: 0.05, MinSilenceNS: 1}
	ranges, err := Detect(samples, 0, opt)
	if err != nil {
		t.Fatal(err)
	}
	var covered int64
	for _, r := range ranges {
		covered += r.EndNS - r.StartNS
	}
	total := tl.EndNS - tl.StartNS
	if covered != total {
		t.Fatalf("silent coverage %d != track span %d (boundary sample double-counted)", covered, total)
	}
	// Ranges must not overlap and must be adjacent or separated.
	for i := 1; i < len(ranges); i++ {
		if ranges[i].StartNS < ranges[i-1].EndNS {
			t.Fatalf("overlapping ranges: %+v", ranges[:i+1])
		}
	}
}

func TestSilenceWindowAlignmentWithOffsetStart(t *testing.T) {
	vals := []float64{0.0, 0.0, 0.0, 0.0}
	tl, frames := audioTimeline(t, vals, 10_000_000, 3_000_000)
	samples, _ := Collect(tl, frames)
	ranges, err := Detect(samples, tl.StartNS, Options{WindowNS: 20_000_000, Threshold: 0.05, MinSilenceNS: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 {
		t.Fatalf("ranges = %+v", ranges)
	}
	if ranges[0].StartNS%20_000_000 != tl.StartNS%20_000_000 {
		t.Fatalf("window not aligned to target start: %+v", ranges[0])
	}
}

func TestDetectRejectsBadSample(t *testing.T) {
	_, err := Detect([]EnergySample{{TrackID: "mic", StartNS: 5, EndNS: 5}}, 0, DefaultOptions())
	if !model.IsKind(err, model.KindInvariant) {
		t.Fatalf("got %v", err)
	}
}

func TestCollectIgnoresVideoTracks(t *testing.T) {
	tr := model.Track{ID: "cam", Kind: model.KindVideo, Timescale: 1000, ClockBits: 32, NominalTickDelta: 40}
	frames := []model.Frame{{TrackID: "cam", Seq: 0, RawTS: 1, Energy: []float64{0}}}
	tl, err := clock.Build([]model.Track{tr}, map[string][]model.Frame{"cam": frames}, clock.Options{NoDriftCorrection: true})
	if err != nil {
		t.Fatal(err)
	}
	samples, err := Collect(tl, map[string][]model.Frame{"cam": frames})
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 0 {
		t.Fatalf("video produced %d audio samples", len(samples))
	}
}
