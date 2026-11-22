package sync

import (
	"testing"

	"mediakernel/internal/clock"
	"mediakernel/internal/model"
)

func TestMissingAndRegressiveTimestampsDegrade(t *testing.T) {
	a := NewAligner(DefaultAlignConfig(), model.NewLog(64))
	f0 := &model.VideoFrame{FrameID: 0, PTS: clock.Milliseconds(100)}
	pts, degraded, reason := a.ValidateVideo(f0)
	if degraded || pts != clock.Milliseconds(100) {
		t.Fatal("valid timestamp must pass through")
	}
	// Missing PTS is synthesized monotonically.
	missing := &model.VideoFrame{FrameID: 1, PTS: 0}
	pts, degraded, reason = a.ValidateVideo(missing)
	if !degraded || pts != clock.Milliseconds(100)+DefaultAlignConfig().FrameStep || reason == "" {
		t.Fatalf("missing PTS policy wrong pts=%v degraded=%v", pts, degraded)
	}
	// Regressive PTS is replaced by a monotonic projection.
	back := &model.VideoFrame{FrameID: 2, PTS: clock.Milliseconds(50)}
	pts2, degraded2, _ := a.ValidateVideo(back)
	if !degraded2 || pts2 <= pts {
		t.Fatalf("regressive PTS not repaired: %v <= %v", pts2, pts)
	}
	if a.DegradedCount() != 2 {
		t.Fatalf("degradation count: %d", a.DegradedCount())
	}
}

func TestBoundaryCrossingClassified(t *testing.T) {
	cfg := DefaultAlignConfig()
	a := NewAligner(cfg, model.NewLog(64))
	a.ValidateVideo(&model.VideoFrame{FrameID: 0, PTS: clock.Milliseconds(2000)})
	// A huge backward jump exceeding the boundary is a clock crossing.
	pts, degraded, reason := a.ValidateVideo(&model.VideoFrame{FrameID: 1, PTS: clock.Milliseconds(10)})
	if !degraded || reason == "" {
		t.Fatal("boundary crossing must be explicitly degraded")
	}
	if pts < clock.Milliseconds(2000) {
		t.Fatal("crossing must be remapped forward onto the timeline")
	}
}

func TestAlignDriftAndMissingAudio(t *testing.T) {
	cfg := DefaultAlignConfig()
	log := model.NewLog(64)
	a := NewAligner(cfg, log)
	frame := model.VideoFrame{FrameID: 1, PTS: clock.Milliseconds(100)}
	// No audio at all: degradation and recorded sync error.
	out := a.Align(frame, frame.PTS, false, "", nil)
	if !out.Degraded || log.CountBy(model.CatSync) == 0 {
		t.Fatal("missing audio must degrade and be logged")
	}
	// Close audio: small drift, no extra drift fault.
	audio := &model.AudioBlock{Start: clock.Milliseconds(105), Samples: []int16{1, 2}, Channels: 1}
	out2 := a.Align(frame, frame.PTS, false, "", audio)
	if out2.DriftMS != 5 {
		t.Fatalf("drift measurement wrong: %d", out2.DriftMS)
	}
	// Far audio: drift constraint violation logged.
	far := &model.AudioBlock{Start: clock.Milliseconds(300), Samples: []int16{1}, Channels: 1}
	a.Align(frame, frame.PTS, false, "", far)
	if a.SyncErrors() < 2 {
		t.Fatalf("drift beyond constraint not counted: %d", a.SyncErrors())
	}
	if len(a.DriftSamples()) != 3 {
		t.Fatal("drift history must be retained for p95")
	}
}
