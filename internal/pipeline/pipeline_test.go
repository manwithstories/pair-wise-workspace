package pipeline

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"mediadeliver/internal/fixture"
	"mediadeliver/internal/manifest"
	"mediadeliver/internal/model"
	"mediadeliver/internal/remux"
)

const testManifestText = `
MATERIAL cam1 cam1.mp4 mp4
TRACK cam1 1 video avc1
TRACK cam1 2 audio mp4a
MATERIAL cam2 cam2.mp4 mp4
TRACK cam2 1 video avc1
TRACK cam2 2 audio mp4a
MATERIAL rec1 rec1.rtp rtp
TRACK rec1 11 audio mp4a
EDIT segA cam1 1 0 8 90000
EDIT segA cam1 2 0 8 48000
MAP segA video cam1 1
MAP segA audio cam1 2
EDIT segB cam2 1 0 7 90000
EDIT segB cam2 2 0 7 48000
MAP segB video cam2 1
MAP segB audio cam2 2
EDIT segC rec1 11 0 8 1
MAP segC audio rec1 11
`

// buildSources creates deterministic in-memory materials matching the manifest.
func buildSources(t *testing.T) map[string]*MaterialSource {
	t.Helper()
	rng := fixture.SeededRand(42)
	cam1 := fixture.BuildMP4([]fixture.TrackSpec{
		{ID: 1, Video: true, Codec: "avc1", Timescale: 90000, Samples: 8, KeyInterval: 1, PayloadBytes: 32},
		{ID: 2, Video: false, Codec: "mp4a", Timescale: 48000, Samples: 8, PayloadBytes: 24},
	}, rng)
	cam2 := fixture.BuildMP4([]fixture.TrackSpec{
		{ID: 1, Video: true, Codec: "avc1", Timescale: 90000, Samples: 8, KeyInterval: 1, PayloadBytes: 32},
		{ID: 2, Video: false, Codec: "mp4a", Timescale: 48000, Samples: 8, PayloadBytes: 24},
	}, rng)
	rtp := fixture.BuildRTPCapture(fixture.RTPSequence(8, 0, 1, 0x1111, 0))
	return map[string]*MaterialSource{
		"cam1": {Material: mustMaterial(t, "cam1", "cam1.mp4", "mp4"), Data: cam1, Format: "mp4"},
		"cam2": {Material: mustMaterial(t, "cam2", "cam2.mp4", "mp4"), Data: cam2, Format: "mp4"},
		"rec1": {Material: mustMaterial(t, "rec1", "rec1.rtp", "rtp"), Data: rtp, Format: "rtp"},
	}
}

func mustMaterial(t *testing.T, id, path, format string) *manifest.Material {
	t.Helper()
	man, err := manifest.Parse(stringReader(testManifestText))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range man.Materials {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("material %s not found", id)
	return nil
}

func baseConfig() Config {
	return Config{
		QueueCapacity:     8,
		MaxPacketBytes:    4096,
		SlowConsumerDelay: time.Millisecond,
		CorruptEvery:      7,
	}
}

func TestRunEndToEndMetrics(t *testing.T) {
	man, err := manifest.Parse(stringReader(testManifestText))
	if err != nil {
		t.Fatal(err)
	}
	rn := NewRunner(filepath.Join(t.TempDir(), "cp.json"), baseConfig())
	rep, err := rn.Run(context.Background(), man, []byte(testManifestText), buildSources(t))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.ParseConsistency != 100.0 {
		t.Fatalf("parse consistency: %v", rep.ParseConsistency)
	}
	if rep.KeyframeHitRate != 100.0 {
		t.Fatalf("keyframe hit rate: %v", rep.KeyframeHitRate)
	}
	if len(rep.Segments) < 3 {
		t.Fatalf("want >=3 segments, got %d", len(rep.Segments))
	}
	for _, rec := range rep.Segments {
		if len(rec.SHA256Hex) != 64 {
			t.Fatalf("segment %d digest not 64 hex: %q", rec.Index, rec.SHA256Hex)
		}
		if rec.PacketCount <= 0 || len(rec.Ranges) == 0 {
			t.Fatalf("segment %d missing provenance: %+v", rec.Index, rec)
		}
	}
	if rep.BackpressureEvents <= 0 {
		t.Fatalf("slow consumer must produce backpressure events, got %d", rep.BackpressureEvents)
	}
	// Bounded memory: peak packets never exceed queue capacity.
	if rep.PeakBufferPackets > int64(baseConfig().QueueCapacity) {
		t.Fatalf("peak buffered %d > capacity %d", rep.PeakBufferPackets, baseConfig().QueueCapacity)
	}
	if rep.RecoveredSegments < 1 {
		t.Fatalf("want recovered segments >=1, got %d", rep.RecoveredSegments)
	}
}

func TestRepeatedRunDeterministic(t *testing.T) {
	man, _ := manifest.Parse(stringReader(testManifestText))
	src := buildSources(t)

	r1 := NewRunner(filepath.Join(t.TempDir(), "a.json"), baseConfig())
	rep1, err := r1.Run(context.Background(), man, []byte(testManifestText), src)
	if err != nil {
		t.Fatal(err)
	}
	r2 := NewRunner(filepath.Join(t.TempDir(), "b.json"), baseConfig())
	rep2, err := r2.Run(context.Background(), man, []byte(testManifestText), src)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep1.Segments) != len(rep2.Segments) {
		t.Fatalf("segment count differs across runs")
	}
	for i := range rep1.Segments {
		if rep1.Segments[i].SHA256Hex != rep2.Segments[i].SHA256Hex {
			t.Fatalf("segment %d digest differs across runs", i)
		}
		if rep1.Segments[i].PacketCount != rep2.Segments[i].PacketCount {
			t.Fatalf("segment %d packet count differs", i)
		}
	}
	if rep1.KeyframeHitRate != rep2.KeyframeHitRate {
		t.Fatalf("hit rate differs: %v vs %v", rep1.KeyframeHitRate, rep2.KeyframeHitRate)
	}
}

func TestResumeSkipsCompletedSegments(t *testing.T) {
	dir := t.TempDir()
	cpPath := filepath.Join(dir, "cp.json")
	man, _ := manifest.Parse(stringReader(testManifestText))
	src := buildSources(t)

	first := NewRunner(cpPath, baseConfig())
	rep1, err := first.Run(context.Background(), man, []byte(testManifestText), src)
	if err != nil {
		t.Fatal(err)
	}
	// Rerun against the same checkpoint and identical inputs: must resume.
	second := NewRunner(cpPath, baseConfig())
	rep2, err := second.Run(context.Background(), man, []byte(testManifestText), src)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if len(rep2.Segments) != len(rep1.Segments) {
		t.Fatalf("resumed segment count mismatch")
	}
	// Checkpoint must still list every segment as complete.
	if len(rep2.Checkpoint.Completed) != len(rep1.Segments) {
		t.Fatalf("checkpoint completion count: %d", len(rep2.Checkpoint.Completed))
	}
}

func TestResumeRejectsChangedInput(t *testing.T) {
	cpPath := filepath.Join(t.TempDir(), "cp.json")
	man, _ := manifest.Parse(stringReader(testManifestText))
	src := buildSources(t)
	first := NewRunner(cpPath, baseConfig())
	if _, err := first.Run(context.Background(), man, []byte(testManifestText), src); err != nil {
		t.Fatal(err)
	}
	// Mutate one source byte; the input digest must now mismatch.
	src2 := buildSources(t)
	src2["cam1"].Data = append([]byte(nil), src2["cam1"].Data...)
	src2["cam1"].Data[10] ^= 0xff
	second := NewRunner(cpPath, baseConfig())
	_, err := second.Run(context.Background(), man, []byte(testManifestText), src2)
	if model.KindOf(err) != model.KindConflict {
		t.Fatalf("changed input must be KindConflict, got %v", err)
	}
}

func TestCorruptMaterialIsolated(t *testing.T) {
	man, _ := manifest.Parse(stringReader(testManifestText))
	src := buildSources(t)
	cfg := baseConfig()
	cfg.CorruptMaterial = map[string]bool{"cam2": true}
	cfg.CorruptEvery = 0 // isolate the write-fault path from the isolation path
	rn := NewRunner(filepath.Join(t.TempDir(), "cp.json"), cfg)
	rep, err := rn.Run(context.Background(), man, []byte(testManifestText), src)
	if err != nil {
		t.Fatalf("isolation should not fail the run: %v", err)
	}
	if len(rep.IsolatedTracks) != 1 || rep.IsolatedTracks[0] != "cam2" {
		t.Fatalf("cam2 must be isolated, got %v", rep.IsolatedTracks)
	}
	if rep.ParseConsistency >= 100.0 {
		t.Fatalf("parse consistency should drop below 100 with a corrupt material, got %v",
			rep.ParseConsistency)
	}
	if len(rep.TrackErrors) == 0 {
		t.Fatal("expected isolated track error records")
	}
	// segA (cam1) and segC (rec1) still deliver; segB lost its only material.
	if len(rep.Segments) < 2 {
		t.Fatalf("healthy segments must still complete, got %d", len(rep.Segments))
	}
}

func TestCancellationPropagates(t *testing.T) {
	man, _ := manifest.Parse(stringReader(testManifestText))
	src := buildSources(t)
	cfg := baseConfig()
	cfg.CorruptEvery = 0
	rn := NewRunner(filepath.Join(t.TempDir(), "cp.json"), cfg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := rn.Run(ctx, man, []byte(testManifestText), src)
	if model.KindOf(err) != model.KindCancelled {
		t.Fatalf("want KindCancelled, got %v", err)
	}
}

// alwaysFailWriter fails every write, modeling a permanently unavailable
// archive disk that the writer cannot make progress against.
type alwaysFailWriter struct{}

func (alwaysFailWriter) Write([]byte) (int, error) {
	return 0, model.New(model.KindIO, "pipeline", "write", "permanent archive fault")
}

func TestPermanentWriterErrorIsClassifiedIO(t *testing.T) {
	// Exercise SegmentWriter directly with a permanently failing sink so no
	// retry can succeed; the error must be KindIO and carry a packet position.
	sw := remux.NewSegmentWriter(0, "segA", nil, alwaysFailWriter{}, model.DefaultLimits())
	err := sw.WritePacket(context.Background(), &model.Packet{
		SourceID: "cam1", TrackID: 1, Seq: 3, Payload: []byte{1, 2, 3},
	})
	if model.KindOf(err) != model.KindIO {
		t.Fatalf("want KindIO for permanent write fault, got %v", err)
	}
	if model.PositionOf(err) != 3 {
		t.Fatalf("write fault must carry the packet position, got %d", model.PositionOf(err))
	}
	// The failed segment is never closed/committed, so it cannot be resumed.
	if _, err := sw.Close(); err != nil {
		t.Fatalf("close of a failed writer should still finalize: %v", err)
	}
}
