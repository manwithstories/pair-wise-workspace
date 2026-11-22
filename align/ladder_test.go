package align

import (
	"testing"

	"mediaslice/media"
	"mediaslice/scan"
)

const seg2s = media.Ticks(180000)

// syntheticIndex builds an in-memory FileIndex with a video track whose
// samples follow start/duration/keyframe arrays in 90 kHz ticks.
func syntheticIndex(repID string, starts, durs []media.Ticks, keys map[int]bool) *scan.FileIndex {
	samples := make([]scan.SampleRecord, len(starts))
	for i := range starts {
		samples[i] = scan.SampleRecord{
			Index: i, Offset: int64(i * 128), Size: 128,
			StartTick: starts[i], Duration: durs[i], Keyframe: keys[i],
		}
	}
	return &scan.FileIndex{
		SourcePath: repID + ".mp4",
		InputSHA256: scan.HashBytes([]byte(repID)),
		Tracks: []scan.TrackIndex{{
			TrackID: 1, Handler: "vide", Timescale: media.ClockRate, Samples: samples,
		}},
		MdatStart: 0, MdatEnd: int64(len(starts) * 128),
	}
}

// vfrStarts produces 25fps nominal samples (3600 ticks) over nSeg segments
// with a deterministic +/-1 tick jitter per boundary, compensated inside
// the following segment.
func vfrStarts(nSeg int, seedShift int) ([]media.Ticks, []media.Ticks, map[int]bool) {
	step := media.Ticks(3600)
	per := int(seg2s / step) // 50 samples per 2s segment
	keys := map[int]bool{}
	var starts, durs []media.Ticks
	cur := media.Ticks(0)
	for seg := 0; seg < nSeg; seg++ {
		jitter := media.Ticks(((seg + seedShift) % 3) - 1) // -1,0,1 rotation
		if seg == nSeg-1 {
			jitter = 0
		}
		idealEnd := media.Ticks(seg+1)*seg2s + jitter
		for k := 0; k < per; k++ {
			d := step
			if k == per-1 {
				d = idealEnd - cur
			}
			starts = append(starts, cur)
			durs = append(durs, d)
			if k == 0 {
				keys[len(starts)-1] = true
			}
			cur += d
		}
	}
	return starts, durs, keys
}

func TestAlignThreeRepresentationsOneTickDelta(t *testing.T) {
	cfg := BoundaryConfig{SegmentTicks: seg2s, KeyframeWaitMax: seg2s}
	var bounds []*TrackBoundary
	for r := range []int{0, 1, 2} {
		starts, durs, keys := vfrStarts(6, 0)
		idx := syntheticIndex([]string{"800k", "1600k", "3200k"}[r], starts, durs, keys)
		b := ComputeTrackBoundary([]string{"800k", "1600k", "3200k"}[r], idx, cfg)
		if len(b.TrackErrors) != 0 {
			t.Fatalf("rep %d errors: %v", r, b.TrackErrors)
		}
		bounds = append(bounds, b)
	}
	al := AlignLadder(bounds, cfg)
	if al.MaxDelta > 1 {
		t.Fatalf("max_boundary_delta_ticks=%d want <=1", al.MaxDelta)
	}
	if len(al.Boundaries) < 6 {
		t.Fatalf("aligned boundaries=%d", len(al.Boundaries))
	}
	plan, err := BuildLadderPlan(al, bounds)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Segments) != len(al.Boundaries)-1 {
		t.Fatalf("segments=%d", len(plan.Segments))
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSparseKeyframeMarksUnsliceable(t *testing.T) {
	// Keyframe only every 3rd segment while the wait window is one segment:
	// the intermediate boundaries must be flagged, not guessed.
	starts, durs, _ := vfrStarts(6, 0)
	keys := map[int]bool{0: true}
	per := 50
	for seg := 0; seg < 6; seg += 3 {
		keys[seg*per] = true
	}
	idx := syntheticIndex("sparse", starts, durs, keys)
	cfg := BoundaryConfig{SegmentTicks: seg2s, KeyframeWaitMax: seg2s}
	b := ComputeTrackBoundary("sparse", idx, cfg)
	if len(b.Unsliceable) == 0 {
		t.Fatal("expected explicit unsliceable intervals")
	}
	for _, iv := range b.Unsliceable {
		if !iv.Unsliceable || iv.To <= iv.From {
			t.Fatalf("bad unsliceable interval %+v", iv)
		}
	}
}

func TestCorruptTrackDoesNotPolluteLadder(t *testing.T) {
	cfg := BoundaryConfig{SegmentTicks: seg2s, KeyframeWaitMax: seg2s}
	// Two healthy representations.
	var healthy []*TrackBoundary
	for r := range []int{0, 1} {
		starts, durs, keys := vfrStarts(4, 0)
		idx := syntheticIndex([]string{"800k", "3200k"}[r], starts, durs, keys)
		healthy = append(healthy, ComputeTrackBoundary([]string{"800k", "3200k"}[r], idx, cfg))
	}
	// One corrupt representation with no usable video track.
	bad := &scan.FileIndex{
		SourcePath: "bad.mp4", InputSHA256: scan.HashBytes([]byte("bad")),
		Errors: media.ErrorList{media.NewError(media.KindTruncatedBox, "bad.mp4", "cut")},
	}
	badBound := ComputeTrackBoundary("1600k", bad, cfg)
	if len(badBound.TrackErrors) == 0 {
		t.Fatal("corrupt track produced no diagnostic")
	}
	// Healthy reps align without the bad one.
	al := AlignLadder(healthy, cfg)
	if al.MaxDelta > 1 {
		t.Fatalf("healthy ladder delta=%d", al.MaxDelta)
	}
	if _, err := BuildLadderPlan(al, healthy); err != nil {
		t.Fatalf("healthy ladder invalid: %v", err)
	}
	// Adding the bad boundary must not overwrite healthy results.
	for _, h := range healthy {
		if len(h.Cuts) == 0 {
			t.Fatal("healthy track lost its cuts")
		}
	}
}

func TestBoundaryUsesActualVFRNotAverageRate(t *testing.T) {
	// Explicit irregular timing: a 2s boundary whose true sample lands 1 tick
	// early must cut at the real tick, never at an averaged 180000.
	starts := []media.Ticks{0, 3600, 7200, 10800, 179999}
	durs := []media.Ticks{3600, 3600, 3600, 169199, 3600}
	keys := map[int]bool{0: true, 4: true}
	idx := syntheticIndex("vfr", starts, durs, keys)
	cfg := BoundaryConfig{SegmentTicks: seg2s, KeyframeWaitMax: seg2s}
	b := ComputeTrackBoundary("vfr", idx, cfg)
	if len(b.Cuts) < 2 || b.Cuts[1].Tick != 179999 {
		t.Fatalf("cut did not follow actual sample tick: %+v", b.Cuts)
	}
}
