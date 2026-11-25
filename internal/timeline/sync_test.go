package timeline_test

import (
	"testing"

	"avcheck/internal/ebml"
	"avcheck/internal/media"
	"avcheck/internal/timeline"
)

func graphFor(t *testing.T, tracks ...ebml.Track) (*ebml.Document, *ebml.Graph) {
	t.Helper()
	doc := ebml.NewDocument()
	doc.Tracks = tracks
	g, err := ebml.NewGraph(doc)
	if err != nil {
		t.Fatalf("graph: %v (%v)", err, doc.Faults)
	}
	return doc, g
}

func samples(track uint64, times ...int64) []timeline.Sample {
	out := make([]timeline.Sample, len(times))
	for i, ts := range times {
		out[i] = timeline.Sample{Track: track, TimeNs: ts, SourceNs: ts}
	}
	return out
}

// TestAlignAnchorSkew feeds a video track offset exactly 20 ms behind audio
// and requires the p95 skew to equal that offset.
func TestAlignAnchorSkew(t *testing.T) {
	doc, g := graphFor(t,
		ebml.Track{Number: 1, Kind: media.KindVideo},
		ebml.Track{Number: 2, Kind: media.KindAudio},
	)
	in := map[uint64][]timeline.Sample{
		1: samples(1, 20e6, 120e6, 220e6),
		2: samples(2, 0, 100e6, 200e6),
	}
	res, err := timeline.Align(doc, g, in, nil, timeline.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Anchor != 2 {
		t.Fatalf("anchor = %d, want 2", res.Anchor)
	}
	if res.AVSkewP95Ns != 20e6 {
		t.Fatalf("p95 skew = %d, want %d", res.AVSkewP95Ns, int64(20e6))
	}
	if res.AVSkewMaxNs != 20e6 {
		t.Fatalf("max skew = %d, want %d", res.AVSkewMaxNs, int64(20e6))
	}
}

// TestAlignReportsSkewAnomaly requires a skew above the threshold to surface
// as a degraded fault rather than being silently swallowed.
func TestAlignReportsSkewAnomaly(t *testing.T) {
	doc, g := graphFor(t,
		ebml.Track{Number: 1, Kind: media.KindVideo},
		ebml.Track{Number: 2, Kind: media.KindAudio},
	)
	in := map[uint64][]timeline.Sample{
		1: samples(1, 500e6),
		2: samples(2, 0),
	}
	res, err := timeline.Align(doc, g, in, nil, timeline.Options{MaxSkewNs: 100e6})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Faults.Has(media.CodeSyncMismatch) {
		t.Fatalf("faults = %v, want sync_mismatch", res.Faults)
	}
	f, _ := res.Faults.First(media.CodeSyncMismatch)
	if f.Level != media.LevelDegraded {
		t.Fatalf("level = %v, want degraded", f.Level)
	}
}

// TestUndeclaredTrackSampleIsAbort requires samples for an unknown track to
// stop the run instead of being dropped.
func TestUndeclaredTrackSampleIsAbort(t *testing.T) {
	doc, g := graphFor(t,
		ebml.Track{Number: 1, Kind: media.KindAudio},
	)
	in := map[uint64][]timeline.Sample{
		1: samples(1, 0),
		9: samples(9, 0),
	}
	_, err := timeline.Align(doc, g, in, nil, timeline.Options{})
	if err == nil {
		t.Fatal("expected an unknown-track error")
	}
	if !doc.Faults.Has(media.CodeUnknownTrack) {
		t.Fatalf("faults = %v, want unknown_track", doc.Faults)
	}
	if doc.Faults.MaxLevel() != media.LevelAbort {
		t.Fatalf("level = %v, want abort", doc.Faults.MaxLevel())
	}
}

// TestNonMonotonicTimestampsAreReported requires out-of-order input to be
// stabilised for output but still reported as a fault.
func TestNonMonotonicTimestampsAreReported(t *testing.T) {
	doc, g := graphFor(t,
		ebml.Track{Number: 1, Kind: media.KindVideo},
		ebml.Track{Number: 2, Kind: media.KindAudio},
	)
	in := map[uint64][]timeline.Sample{
		1: samples(1, 200e6, 100e6, 300e6),
		2: samples(2, 0, 100e6, 200e6),
	}
	res, err := timeline.Align(doc, g, in, nil, timeline.Options{})
	if err != nil {
		t.Fatal(err)
	}
	vt, ok := res.Track(1)
	if !ok {
		t.Fatal("video timeline missing")
	}
	if vt.Monotonic {
		t.Fatal("timeline should be flagged non-monotonic")
	}
	for i := 1; i < len(vt.Samples); i++ {
		if vt.Samples[i].TimeNs < vt.Samples[i-1].TimeNs {
			t.Fatalf("output not stabilised at %d", i)
		}
	}
	if !res.Faults.Has(media.CodeNonMonotonic) {
		t.Fatalf("faults = %v, want non_monotonic_timestamp", res.Faults)
	}
}

// TestGapsCarryNoTime proves an unrecovered gap is counted but never turned
// into a sample with an invented timestamp.
func TestGapsCarryNoTime(t *testing.T) {
	doc, g := graphFor(t,
		ebml.Track{Number: 1, Kind: media.KindVideo},
		ebml.Track{Number: 2, Kind: media.KindAudio},
	)
	in := map[uint64][]timeline.Sample{
		1: samples(1, 0, 100e6),
		2: samples(2, 0, 100e6),
	}
	gaps := map[uint64][]uint32{1: {7, 8, 9}}
	res, err := timeline.Align(doc, g, in, gaps, timeline.Options{})
	if err != nil {
		t.Fatal(err)
	}
	vt, _ := res.Track(1)
	if vt.Gaps != 3 {
		t.Fatalf("gaps = %d, want 3", vt.Gaps)
	}
	if len(vt.Samples) != 2 {
		t.Fatalf("samples = %d, want 2 (gaps must not add samples)", len(vt.Samples))
	}
	for _, s := range vt.Samples {
		if s.TimeNs < 0 || s.TimeNs > 100e6 {
			t.Fatalf("unexpected invented time %d", s.TimeNs)
		}
	}
}

// TestCueDrift measures the hint-misalignment metric independently of skew.
func TestCueDrift(t *testing.T) {
	doc, g := graphFor(t,
		ebml.Track{Number: 1, Kind: media.KindVideo},
		ebml.Track{Number: 2, Kind: media.KindAudio},
		ebml.Track{Number: 3, Kind: media.KindCue, DependsOn: []uint64{1}},
	)
	in := map[uint64][]timeline.Sample{
		1: samples(1, 0, 100e6, 200e6),
		2: samples(2, 0, 100e6),
		3: samples(3, 105e6, 195e6),
	}
	res, err := timeline.Align(doc, g, in, nil, timeline.Options{})
	if err != nil {
		t.Fatal(err)
	}
	drift := res.CueDrift()
	if len(drift) != 2 {
		t.Fatalf("drift len = %d, want 2", len(drift))
	}
	if drift[0] != 5e6 || drift[1] != -5e6 {
		t.Fatalf("drift = %v, want [5ms -5ms]", drift)
	}
	// The cue track must flush after its dependency.
	if len(res.CueOrder) == 0 || res.CueOrder[len(res.CueOrder)-1] != 3 {
		t.Fatalf("cue order = %v, want cue last", res.CueOrder)
	}
}

func TestAnchorWithoutSamplesIsDegraded(t *testing.T) {
	doc, g := graphFor(t,
		ebml.Track{Number: 1, Kind: media.KindVideo},
		ebml.Track{Number: 2, Kind: media.KindAudio},
	)
	in := map[uint64][]timeline.Sample{1: samples(1, 0, 100e6)}
	res, err := timeline.Align(doc, g, in, nil, timeline.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Faults.Has(media.CodeSyncAnchor) {
		t.Fatalf("faults = %v, want sync_anchor_missing", res.Faults)
	}
}
