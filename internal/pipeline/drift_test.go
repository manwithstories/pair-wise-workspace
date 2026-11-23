package pipeline

import (
	"testing"

	"mediarun/internal/clock"
)

// TestDriftFitScopedToSample checks that the anchors produced by the
// alignment stage for a 48-second damaged sample yield a uniquely determined,
// fast-running audio clock fit.
func TestDriftFitScopedToSample(t *testing.T) {
	s, media, err := buildSample(Config{
		Seed: 42, Segments: 12, SegmentDurNS: 4_000_000_000, PacketRate: 50,
		Faults: Faults{Drift: true}, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if len(media.Segments) == 0 {
		t.Fatal("no segments")
	}
	j := runJitter(s)
	syncOut := runSync(s, j)
	if syncOut.Drift == nil {
		t.Fatalf("drift not fitted: %s", syncOut.DriftErr)
	}
	// The venue clock is 300ppm fast: reference time runs 300 microseconds
	// per second ahead of media time, so the fitted slope is 0.9997e9 ppb.
	want := int64(1_000_000_000 - 300_000)
	if d := syncOut.Drift.SlopePPB - want; d < -2_000 || d > 2_000 {
		t.Fatalf("slope = %d ppb, want ~%d", syncOut.Drift.SlopePPB, want)
	}
}

// TestAmbiguousDriftFailsFit pins the refusal behaviour: anchors with no
// usable baseline must not produce a fabricated slope.
func TestAmbiguousDriftFailsFit(t *testing.T) {
	pts := []clock.DriftPoint{{RefNS: 0, MediaNS: 0}, {RefNS: 1, MediaNS: 1},
		{RefNS: 2, MediaNS: 2}, {RefNS: 3, MediaNS: 3}}
	if _, err := clock.FitDrift(pts, clock.DefaultFitConfig()); err == nil {
		t.Fatalf("degenerate baseline produced a fit")
	}
}
