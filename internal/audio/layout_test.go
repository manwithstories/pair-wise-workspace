package audio

import (
	"testing"

	"github.com/example/mediad/internal/ingest"
	"github.com/example/mediad/internal/model"
)

func TestValidateLayout(t *testing.T) {
	ok := &ingest.AudioTrackLayout{
		TrackID: 1, SampleRate: 48000, Channels: 2,
		ChannelLayout: []string{"L", "R"},
	}
	if err := ValidateLayout(ok); err != nil {
		t.Fatalf("stereo must pass: %v", err)
	}

	mismatch := &ingest.AudioTrackLayout{
		Channels: 4, ChannelLayout: []string{"L", "R"},
	}
	if err := ValidateLayout(mismatch); err == nil {
		t.Fatal("count/label mismatch must be rejected")
	}

	unsupported := &ingest.AudioTrackLayout{
		Channels: 4, ChannelLayout: []string{"L", "R", "Bs", "Ls"},
	}
	err := ValidateLayout(unsupported)
	if me, ok := model.AsError(err); !ok || me.Kind != model.KindUnsupported {
		t.Fatalf("unsupported layout error = %v", err)
	}
}

func TestPlanMapping(t *testing.T) {
	a := &ingest.AudioTrackLayout{
		TrackID: 2, SampleRate: 44100, Channels: 1,
		ChannelLayout: []string{"M"},
	}
	m, err := PlanMapping(a)
	if err != nil {
		t.Fatal(err)
	}
	if m.OutputRate != 44100 || m.NeedsResample {
		t.Fatalf("standard-rate mapping = %+v", m)
	}

	odd := &ingest.AudioTrackLayout{
		TrackID: 3, SampleRate: 11025, Channels: 1,
		ChannelLayout: []string{"M"},
	}
	m2, err := PlanMapping(odd)
	if err != nil {
		t.Fatal(err)
	}
	if !m2.NeedsResample || m2.OutputRate != 48000 {
		t.Fatalf("resample mapping = %+v", m2)
	}
}

func TestResampleBoundary(t *testing.T) {
	m := &Mapping{InputRate: 44100, OutputRate: 48000,
		NeedsResample: true}

	// 44100 input samples = exactly 48000 output samples: divisible.
	if err := m.CheckResampleBoundary([][2]uint64{{0, 44099}}); err != nil {
		t.Fatalf("exact boundary rejected: %v", err)
	}

	// A one-sample interval cannot map to an integer output count.
	err := m.CheckResampleBoundary([][2]uint64{{0, 0}})
	if err == nil {
		t.Fatal("indivisible boundary must be rejected")
	}
	me, ok := model.AsError(err)
	if !ok || me.Kind != model.KindUnsupported {
		t.Fatalf("wrong error: %v", err)
	}
	if len(m.RejectedRanges) != 1 || m.RejectedRanges[0][0] != 0 {
		t.Fatalf("rejected ranges = %v", m.RejectedRanges)
	}
}

func TestPassthroughSkipsBoundaryCheck(t *testing.T) {
	m := &Mapping{InputRate: 48000, OutputRate: 48000}
	if err := m.CheckResampleBoundary([][2]uint64{{0, 1}, {5, 9}}); err != nil {
		t.Fatalf("passthrough must never reject: %v", err)
	}
}

func TestAbnormalLayoutFromFixture(t *testing.T) {
	// Mirrors the DamageBadLayout fixture contract: a track whose
	// declared channel count disagrees with its label set.
	bad := &ingest.AudioTrackLayout{
		TrackID: 9, SampleRate: 48000, Channels: 3,
		ChannelLayout: []string{"L", "R"},
	}
	if _, err := PlanMapping(bad); err == nil {
		t.Fatal("abnormal layout fixture must be rejected before commit")
	}
}
