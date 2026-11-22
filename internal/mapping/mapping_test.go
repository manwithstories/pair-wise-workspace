package mapping_test

import (
	"testing"

	"mediarun/internal/ingest"
	"mediarun/internal/mapping"
	"mediarun/internal/mediaerr"
)

func sampleTracks() []ingest.TrackDesc {
	return []ingest.TrackDesc{
		{ID: "video/0", Kind: ingest.KindVideo, Codec: "avc1", Timescale: 30000, Width: 1920, Height: 1080, SampleCount: 100},
		{ID: "audio/0", Kind: ingest.KindAudio, Codec: "mp4a", Timescale: 48000, Channels: 2, SampleCount: 200},
	}
}

func TestValidPlanBuildsOrderedTargets(t *testing.T) {
	cfg := mapping.Config{Rules: []mapping.Rule{
		{Target: mapping.RoleAudio, Source: "audio/0"},
		{Target: mapping.RoleVideo, Source: "video/0"},
	}}
	plan, err := mapping.Build(cfg, sampleTracks())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Targets) != 2 || plan.Targets[0].Role != mapping.RoleVideo {
		t.Fatalf("targets not ordered video-first: %+v", plan.Targets)
	}
	if tr, ok := plan.Track("video/0"); !ok || tr.Codec != "avc1" {
		t.Fatalf("track lookup wrong: %+v ok=%v", tr, ok)
	}
}

func TestDuplicateTargetRejected(t *testing.T) {
	cfg := mapping.Config{Rules: []mapping.Rule{
		{Target: mapping.RoleVideo, Source: "video/0"},
		{Target: mapping.RoleVideo, Source: "video/0"},
		{Target: mapping.RoleAudio, Source: "audio/0"},
	}}
	_, err := mapping.Build(cfg, sampleTracks())
	if !mediaerr.Is(err, mediaerr.CodeInvalidConfig) {
		t.Fatalf("want INVALID_CONFIG for duplicate target, got %v", err)
	}
}

func TestMissingRequiredRoleRejected(t *testing.T) {
	cfg := mapping.Config{Rules: []mapping.Rule{
		{Target: mapping.RoleVideo, Source: "video/0"},
	}}
	_, err := mapping.Build(cfg, sampleTracks())
	if !mediaerr.Is(err, mediaerr.CodeInvalidConfig) {
		t.Fatalf("want INVALID_CONFIG for missing audio, got %v", err)
	}
}

func TestUnknownSourceRejected(t *testing.T) {
	cfg := mapping.Config{Rules: []mapping.Rule{
		{Target: mapping.RoleVideo, Source: "video/9"},
		{Target: mapping.RoleAudio, Source: "audio/0"},
	}}
	_, err := mapping.Build(cfg, sampleTracks())
	if !mediaerr.Is(err, mediaerr.CodeInvalidConfig) {
		t.Fatalf("want INVALID_CONFIG for unknown source, got %v", err)
	}
}

func TestRoleKindMismatchRejected(t *testing.T) {
	cfg := mapping.Config{Rules: []mapping.Rule{
		{Target: mapping.RoleAudio, Source: "video/0"}, // video into audio slot
		{Target: mapping.RoleVideo, Source: "video/0"},
	}}
	_, err := mapping.Build(cfg, sampleTracks())
	if !mediaerr.Is(err, mediaerr.CodeInvalidConfig) {
		t.Fatalf("want INVALID_CONFIG for kind mismatch, got %v", err)
	}
}

func TestIncompatibleCodecRejected(t *testing.T) {
	tracks := sampleTracks()
	tracks[1].Codec = "opus" // not in the documented compatible set
	cfg := mapping.Config{Rules: []mapping.Rule{
		{Target: mapping.RoleVideo, Source: "video/0"},
		{Target: mapping.RoleAudio, Source: "audio/0"},
	}}
	_, err := mapping.Build(cfg, tracks)
	if !mediaerr.Is(err, mediaerr.CodeInvalidConfig) {
		t.Fatalf("want INVALID_CONFIG for incompatible codec, got %v", err)
	}
}

func TestMXFSubsetCodecsAccepted(t *testing.T) {
	tracks := []ingest.TrackDesc{
		{ID: "video/0", Kind: ingest.KindVideo, Codec: "vcb1", SampleCount: 10},
		{ID: "audio/1", Kind: ingest.KindAudio, Codec: "pcm1", SampleCount: 10},
	}
	cfg := mapping.Config{Rules: []mapping.Rule{
		{Target: mapping.RoleVideo, Source: "video/0"},
		{Target: mapping.RoleAudio, Source: "audio/1"},
	}}
	if _, err := mapping.Build(cfg, tracks); err != nil {
		t.Fatalf("MXF subset codecs must be accepted: %v", err)
	}
}
