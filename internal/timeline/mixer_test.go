package timeline

import (
	"testing"

	"archive/internal/model"
)

// TestMixerAlignedTracksNoDrift verifies synchronized tracks produce no
// drift events regardless of interleaving.
func TestMixerAlignedTracksNoDrift(t *testing.T) {
	m := NewMixer(50_000_000)
	m.RegisterTrack(1, model.TrackVideo, 90000)
	m.RegisterTrack(2, model.TrackAudio, 48000)
	for ord := int64(0); ord < 500; ord++ {
		vp := model.Packet{SSRC: 1, ExtSequence: ord, Timestamp: uint32(ord) * 900}
		ap := model.Packet{SSRC: 2, ExtSequence: ord, Timestamp: uint32(ord) * 480}
		if ord%25 == 0 {
			vp.Keyframe = true
		}
		if _, err := m.Mix(vp); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Mix(ap); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.Drift()) != 0 {
		t.Errorf("aligned tracks reported %d false drift events", len(m.Drift()))
	}
}

// TestMixerDetectsGenuineDrift verifies a real rate mismatch is reported.
func TestMixerDetectsGenuineDrift(t *testing.T) {
	m := NewMixer(50_000_000)
	m.RegisterTrack(1, model.TrackVideo, 90000)
	m.RegisterTrack(2, model.TrackAudio, 48000)
	for ord := int64(0); ord < 500; ord++ {
		// video runs 20% faster than audio.
		vp := model.Packet{SSRC: 1, ExtSequence: ord, Timestamp: uint32(ord) * 1080}
		ap := model.Packet{SSRC: 2, ExtSequence: ord, Timestamp: uint32(ord) * 480}
		if ord%25 == 0 {
			vp.Keyframe = true
		}
		if _, err := m.Mix(vp); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Mix(ap); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.Drift()) == 0 {
		t.Error("expected drift events for a genuine clock-rate mismatch")
	}
}

// TestMixerProvenance verifies frames retain clock basis and source range.
func TestMixerProvenance(t *testing.T) {
	m := NewMixer(50_000_000)
	m.RegisterTrack(1, model.TrackVideo, 90000)
	// First packet establishes the origin; the second packet's delta maps
	// to exactly one second at 90 kHz.
	if _, err := m.Mix(model.Packet{SSRC: 1, ExtSequence: 41, Timestamp: 0}); err != nil {
		t.Fatal(err)
	}
	f, err := m.Mix(model.Packet{SSRC: 1, ExtSequence: 42, Timestamp: 90000})
	if err != nil {
		t.Fatal(err)
	}
	if f.SourceStart != 42 {
		t.Errorf("source start = %d, want 42", f.SourceStart)
	}
	if f.ClockBasis == "" {
		t.Error("clock basis must be recorded for audit")
	}
	if f.TimeNanos != 1_000_000_000 {
		t.Errorf("time = %d, want 1s", f.TimeNanos)
	}
}

// TestMixerMissingReport verifies a track without SR is listed.
func TestMixerMissingReport(t *testing.T) {
	m := NewMixer(50_000_000)
	m.RegisterTrack(1, model.TrackVideo, 90000)
	if _, err := m.Mix(model.Packet{SSRC: 1, Timestamp: 0}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ssrc := range m.MissingReports() {
		if ssrc == 1 {
			found = true
		}
	}
	if !found {
		t.Error("track without SR should appear in MissingReports")
	}
}
