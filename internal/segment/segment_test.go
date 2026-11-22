package segment_test

import (
	"path/filepath"
	"testing"

	"mediac/internal/clock"
	"mediac/internal/fixture"
	"mediac/internal/probe"
	"mediac/internal/segment"
)

func probeSpec(t *testing.T, spec fixture.Spec) *probe.Result {
	t.Helper()
	dir := t.TempDir()
	if _, err := fixture.WriteFile(dir, "f.mp4", spec); err != nil {
		t.Fatal(err)
	}
	res, merr := probe.Probe(filepath.Join(dir, "f.mp4"))
	if merr != nil {
		t.Fatalf("probe: %v", merr)
	}
	return res
}

func reps(t *testing.T, bitrates ...int) []*probe.Result {
	var out []*probe.Result
	for _, b := range bitrates {
		out = append(out, probeSpec(t, fixture.Standard(b, 100)))
	}
	return out
}

func TestCommonCutsAligned(t *testing.T) {
	cuts, merr := segment.CommonCuts(reps(t, 1, 2, 3), clock.New(2, 1))
	if merr != nil {
		t.Fatalf("cuts: %v", merr)
	}
	if len(cuts) != 3 {
		t.Fatalf("cuts = %v, want [0 2 4]", cuts)
	}
	for i, want := range []int64{0, 2, 4} {
		if cuts[i].Cmp(clock.New(want, 1)) != 0 {
			t.Fatalf("cuts[%d] = %s, want %ds", i, cuts[i], want)
		}
	}
}

// Audio sample boundary lands exactly on the cut point: accepted with
// zero tolerance, and the cut stays exactly 2000 ms after many grid
// steps (no accumulated drift).
func TestSampleBoundaryExactlyOnCut(t *testing.T) {
	spec := fixture.Standard(1, 1000) // 40 s
	res := probeSpec(t, spec)
	cuts, merr := segment.CommonCuts([]*probe.Result{res, probeSpec(t, fixture.Standard(2, 1000))},
		clock.New(2, 1))
	if merr != nil {
		t.Fatalf("cuts: %v", merr)
	}
	if len(cuts) != 21 {
		t.Fatalf("got %d cuts, want 21", len(cuts))
	}
	last := cuts[len(cuts)-2] // 38 s: 19 grid steps of exactly 2 s
	if ms, ok := last.MillisExact(); !ok || ms != 38000 {
		t.Fatalf("cut drifted: %s (exact=%v)", last, ok)
	}
	// 38 s on the audio grid: 38*48000/960 = 1900 frames exactly.
	a := res.Track("audio")
	if (38*a.Timescale)%a.Samples[0].Duration != 0 {
		t.Fatal("audio grid misaligned")
	}
}

// A 44.1 kHz / 1024-sample audio grid has no common cut with 1 s
// keyframes within 4 s. The algorithm must fail explicitly instead of
// rounding the cut to the nearest frame.
func TestNoSilentRounding(t *testing.T) {
	spec := fixture.Standard(1, 100)
	spec.Tracks[1].SampleRate = 44100
	spec.Tracks[1].Timescale = 44100
	spec.Tracks[1].FrameDuration = 1024
	spec.Tracks[1].SampleCount = 173 // ~4 s of 1024-sample frames
	r1 := probeSpec(t, spec)
	r2 := probeSpec(t, fixture.Standard(2, 100))
	_, merr := segment.CommonCuts([]*probe.Result{r1, r2}, clock.New(1, 1))
	if merr == nil || merr.Code != "no_common_cut" {
		t.Fatalf("expected no_common_cut, got %v", merr)
	}
}

// Keyframe positions differing across representations must shrink the
// common cut set, never shift it.
func TestKeyframeMismatch(t *testing.T) {
	r1 := probeSpec(t, fixture.Standard(1, 100))
	spec := fixture.Standard(2, 100)
	spec.Tracks[0].KeyframeEvery = 30 // keyframes at 1.2 s grid
	r2 := probeSpec(t, spec)
	_, merr := segment.CommonCuts([]*probe.Result{r1, r2}, clock.New(2, 1))
	if merr == nil || merr.Code != "no_common_cut" {
		t.Fatalf("expected no_common_cut, got %v", merr)
	}
}

func TestManifestDeterministic(t *testing.T) {
	dir := t.TempDir()
	m := &segment.Manifest{Version: 1, ConfigSHA256: "cfg", SegmentDurationMs: 2000}
	e, merr := segment.Entry(0, clock.New(0, 1), clock.New(2, 1), "abc", 123, 42)
	if merr != nil {
		t.Fatal(merr)
	}
	m.Representations = []segment.RepManifest{{ID: "rep1", Segments: []segment.SegmentEntry{e}}}
	s1, _ := segment.Write(dir, m)
	s2, _ := segment.Write(dir, m)
	if s1 != s2 {
		t.Fatalf("manifest not deterministic: %s vs %s", s1, s2)
	}
	back, s3, merr := segment.Load(dir)
	if merr != nil || s3 != s1 || back.Representations[0].Segments[0].EndMs != 2000 {
		t.Fatalf("load mismatch: %v %s", merr, s3)
	}
	// Inexact cuts must be rejected, not rounded.
	if _, merr := segment.Entry(0, clock.New(1, 3), clock.New(2, 1), "x", 0, 0); merr == nil {
		t.Fatal("inexact cut was silently accepted")
	}
}
