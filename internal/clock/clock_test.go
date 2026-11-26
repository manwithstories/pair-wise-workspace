package clock

import (
	"errors"
	"testing"

	"mediaflow/internal/fixture"
	"mediaflow/internal/timeline"
)

func TestDriftCorrectionResidual(t *testing.T) {
	clip, err := fixture.Generate(fixture.DefaultConfig(41))
	if err != nil {
		t.Fatal(err)
	}
	norm := NewNormalizer()
	if err := norm.Estimate(clip.Packets, clip.Tracks); err != nil {
		t.Fatal(err)
	}
	maxRes, track := norm.MaxResidual(clip.Packets)
	t.Logf("max residual = %d us on track %d (limit %d)", maxRes, track, DriftLimitUS)
	if maxRes > DriftLimitUS {
		t.Fatalf("drift residual %d us exceeds 20ms", maxRes)
	}
	// The video track is configured with +120 PPM drift; the fitted model must
	// recover a positive drift of the same order.
	m, ok := norm.Model(1)
	if !ok {
		t.Fatal("missing video model")
	}
	if m.DriftPPM <= 0 {
		t.Fatalf("expected positive video drift, got PPM=%d", m.DriftPPM)
	}
}

func TestNormalizeAllVFRPreserved(t *testing.T) {
	clip, _ := fixture.Generate(fixture.DefaultConfig(41))
	norm := NewNormalizer()
	if err := norm.Estimate(clip.Packets, clip.Tracks); err != nil {
		t.Fatal(err)
	}
	out, err := norm.NormalizeAll(clip.Packets)
	if err != nil {
		t.Fatal(err)
	}
	// VFR video frames must retain distinct native durations after normalization.
	var nativeDurs map[int64]bool = map[int64]bool{}
	var count int
	for _, np := range out {
		if np.Track == 1 {
			nativeDurs[np.NativeDur] = true
			count++
		}
	}
	if count == 0 || len(nativeDurs) < 2 {
		t.Fatalf("VFR durations collapsed: frames=%d distinct=%d", count, len(nativeDurs))
	}
	// Normalized positions must be non-negative and ordered per track.
	lastByTrack := map[uint32]int64{}
	for _, np := range out {
		if np.StartUS < 0 {
			t.Fatal("negative normalized position")
		}
		if prev, ok := lastByTrack[np.Track]; ok && np.StartUS < prev {
			t.Fatalf("track %d positions not ordered", np.Track)
		}
		lastByTrack[np.Track] = np.StartUS
	}
}

func TestZeroClockDetected(t *testing.T) {
	// Every packet frozen on the same raw timestamp -> zero clock.
	pkts := []fixture.Packet{
		{Track: 1, Seq: 0, RawTS: 100, TrueUS: 0, Duration: 3600},
		{Track: 1, Seq: 1, RawTS: 100, TrueUS: 40000, Duration: 3600},
	}
	tracks := []fixture.Track{
		{ID: 1, Kind: fixture.KindVideo, Timebase: fixture.NumDen{Num: 1, Den: 90000}},
	}
	norm := NewNormalizer()
	err := norm.Estimate(pkts, tracks)
	if !errors.Is(err, timeline.ErrZeroClock) {
		t.Fatalf("expected zero clock error, got %v", err)
	}
	te, ok := timeline.AsError(err)
	if !ok || te.Track != 1 {
		t.Fatalf("error must identify track: %v", err)
	}
}

func TestNegativeDurationDetected(t *testing.T) {
	clip, _ := fixture.Generate(fixture.DefaultConfig(41))
	norm := NewNormalizer()
	if err := norm.Estimate(clip.Packets, clip.Tracks); err != nil {
		t.Fatal(err)
	}
	bad := fixture.Packet{Track: 1, Seq: 999, RawTS: 90000, TrueUS: 1_000_000, Duration: -1}
	if _, err := norm.Normalize(bad); !errors.Is(err, timeline.ErrNegativeDuration) {
		t.Fatalf("expected negative duration error, got %v", err)
	}
}

func TestUnknownTrackRejected(t *testing.T) {
	norm := NewNormalizer()
	_, err := norm.Normalize(fixture.Packet{Track: 42})
	if err == nil {
		t.Fatal("expected error for unknown track")
	}
}
