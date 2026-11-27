package syncer

import (
	"errors"
	"testing"

	"archive/internal/manifest"
	"archive/internal/model"
)

func testTimeline() *manifest.Timeline {
	return &manifest.Timeline{
		Format: manifest.FormatHLS,
		Segments: []manifest.Segment{
			{Seq: 0, URI: "seg0.ts", Duration: model.Second, Start: 0, End: model.Second},
			{Seq: 1, URI: "seg1.ts", Duration: model.Second,
				Start: model.Second, End: 2 * model.Second, Discontinuity: true},
		},
	}
}

func TestAlignAnchorsAndMonotonicity(t *testing.T) {
	log := model.NewFaultLog()
	a := NewAligner(testTimeline(), log)

	// Video clock starts at an arbitrary RTP-derived offset; alignment must
	// translate the first packet onto the segment origin (t=0).
	v0 := a.Align(TrackVideo, 100, 500*model.Millisecond, false, false)
	if v0.ManifestNano != 0 {
		t.Fatalf("video anchor = %v, want 0", v0.ManifestNano)
	}
	v1 := a.Align(TrackVideo, 101, 600*model.Millisecond, false, false)
	if v1.ManifestNano != 100*model.Millisecond || v1.ManifestNano <= v0.ManifestNano {
		t.Fatalf("video translation/monotonicity broken: %v -> %v", v0.ManifestNano, v1.ManifestNano)
	}
	if v1.SegmentSeq != 0 {
		t.Fatalf("segment lookup wrong: %d", v1.SegmentSeq)
	}

	// Audio runs on its own clock; its anchor maps to its own origin so both
	// tracks share the manifest coordinate system.
	au0 := a.Align(TrackAudio, 1, 900*model.Millisecond, false, false)
	if au0.ManifestNano != 0 {
		t.Fatalf("audio anchor = %v, want 0", au0.ManifestNano)
	}
}

func TestAlignClampsNonMonotonic(t *testing.T) {
	log := model.NewFaultLog()
	a := NewAligner(testTimeline(), log)
	a.Align(TrackVideo, 1, 100*model.Millisecond, false, false)
	a.Align(TrackVideo, 2, 200*model.Millisecond, false, false)
	// A late, out-of-order timestamp must not move the timeline backwards.
	clamped := a.Align(TrackVideo, 3, 50*model.Millisecond, false, false)
	if clamped.ManifestNano <= 100*model.Millisecond {
		t.Fatalf("non-monotonic packet not clamped forward: %v", clamped.ManifestNano)
	}
	if log.Count(model.FaultReorder) != 1 {
		t.Fatal("clamp fault not recorded")
	}
}

func TestFinalizeWithinTolerance(t *testing.T) {
	log := model.NewFaultLog()
	a := NewAligner(testTimeline(), log)
	var out []AlignedPacket
	// Interleaved A/V with sub-20ms skew (video at 0/100ms, audio 12ms/112ms).
	for _, s := range []struct {
		kind TrackKind
		seq  int64
		ts   model.Nano
	}{
		{TrackVideo, 1, 0},
		{TrackAudio, 1, 12 * model.Millisecond},
		{TrackVideo, 2, 100 * model.Millisecond},
		{TrackAudio, 2, 112 * model.Millisecond},
	} {
		out = append(out, a.Align(s.kind, s.seq, s.ts, false, false))
	}
	final, err := a.Finalize(out)
	if err != nil {
		t.Fatalf("in-tolerance alignment failed: %v", err)
	}
	if final[0].Track != TrackVideo || final[0].ManifestNano != 0 {
		t.Fatalf("global order wrong: %+v", final[0])
	}
	if a.MaxError() > MaxSyncError {
		t.Fatalf("max error %v exceeds tolerance", a.MaxError())
	}
	// The discontinuity declared on segment 1 must survive alignment.
	if log.Count(model.FaultDiscontinuity) != 1 {
		t.Fatal("manifest discontinuity not preserved")
	}
}

func TestFinalizeRejectsExcessiveSkew(t *testing.T) {
	log := model.NewFaultLog()
	a := NewAligner(testTimeline(), log)
	// Inject a 40 ms capture-clock skew for audio: audio RTP time 0 maps to
	// manifest time 40 ms, beyond the 20 ms tolerance.
	a.AnchorHint(TrackAudio, 0, 40*model.Millisecond)
	var out []AlignedPacket
	out = append(out, a.Align(TrackVideo, 1, 0, false, false))
	out = append(out, a.Align(TrackVideo, 2, 200*model.Millisecond, false, false))
	out = append(out, a.Align(TrackAudio, 1, 0, false, false))
	out = append(out, a.Align(TrackAudio, 2, 200*model.Millisecond, false, false))
	_, err := a.Finalize(out)
	if !errors.Is(err, model.ErrInvariant) {
		t.Fatalf("excessive skew must be an invariant error, got %v", err)
	}
}
