package audio

import (
	"testing"

	"github.com/mediamend/mediamend/internal/model"
)

func TestCleanSpecNoFaults(t *testing.T) {
	spec := Spec{TrackID: "a1", Layout: "stereo", SampleRate: 48000, FrameSize: 480, FrameCount: 100}
	out, faults := Normalize(spec, Policy{}, "in.jsonl")
	if model.HasFatal(faults) || out.Layout != "stereo" || out.SampleRate != 48000 {
		t.Fatalf("clean spec produced faults: %v", faults)
	}
}

func TestUnknownLayoutAndInvalidRateFatal(t *testing.T) {
	_, faults := Normalize(Spec{TrackID: "a", Layout: "atmos-12", SampleRate: 48000, FrameSize: 1}, Policy{}, "f")
	if len(faults) != 1 || faults[0].Severity != model.SeverityFatal {
		t.Fatalf("unknown layout: %v", faults)
	}
	_, faults = Normalize(Spec{TrackID: "a", Layout: "mono", SampleRate: 12345, FrameSize: 1}, Policy{}, "f")
	if len(faults) != 1 || faults[0].Class != model.FaultAudioLayout {
		t.Fatalf("invalid rate: %v", faults)
	}
}

func TestPermittedDownmixAndResample(t *testing.T) {
	spec := Spec{TrackID: "a", Layout: "5.1", SampleRate: 96000, FrameSize: 960, FrameCount: 50}
	pol := Policy{TargetLayout: "stereo", TargetRate: 48000, AllowDownmix: true, AllowResample: true}
	out, faults := Normalize(spec, pol, "f")
	if model.HasFatal(faults) {
		t.Fatalf("permitted conversion blocked: %v", faults)
	}
	if out.Layout != "stereo" || out.SampleRate != 48000 {
		t.Fatalf("normalization not applied: %+v", out)
	}
	if len(faults) != 2 {
		t.Fatalf("want 2 evidence faults, got %d", len(faults))
	}
}

func TestDisallowedConflictFatal(t *testing.T) {
	spec := Spec{TrackID: "a", Layout: "5.1", SampleRate: 48000, FrameSize: 480, FrameCount: 10}
	// Downmix requested but not permitted.
	_, faults := Normalize(spec, Policy{TargetLayout: "stereo"}, "f")
	if !model.HasFatal(faults) {
		t.Fatalf("unpermitted downmix should be fatal: %v", faults)
	}
	// Resample requested but not permitted.
	_, faults = Normalize(spec, Policy{TargetRate: 44100}, "f")
	if !model.HasFatal(faults) {
		t.Fatalf("unpermitted resample should be fatal: %v", faults)
	}
}

func TestNonDivisibleBoundaryFatal(t *testing.T) {
	// 1 sample at 44100 Hz: 1*90000 % 44100 = 1800 != 0 -> fractional tick.
	spec := Spec{TrackID: "a", Layout: "mono", SampleRate: 44100, FrameSize: 1, FrameCount: 1}
	_, faults := Normalize(spec, Policy{}, "f")
	var boundary bool
	for _, f := range faults {
		if f.Class == model.FaultAudioBoundary {
			boundary = true
		}
	}
	if !boundary {
		t.Fatalf("non-divisible boundary not flagged: %v", faults)
	}
}

func TestDivisibleBoundaryPasses(t *testing.T) {
	// 44100 samples (1s) at 44100 Hz: 44100*90000 % 44100 == 0.
	spec := Spec{TrackID: "a", Layout: "mono", SampleRate: 44100, FrameSize: 44100, FrameCount: 1}
	_, faults := Normalize(spec, Policy{}, "f")
	for _, f := range faults {
		if f.Class == model.FaultAudioBoundary {
			t.Fatalf("clean boundary flagged: %v", f)
		}
	}
}
