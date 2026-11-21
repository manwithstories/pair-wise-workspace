package pipeline

import (
	"context"
	"errors"
	"testing"

	"mediacheck/internal/fixture"
	"mediacheck/internal/model"
	"mediacheck/internal/rtp"
)

func demoInputs(t *testing.T, reps []string, damage func(*fixture.MemFile, *fixture.Layout)) []Input {
	t.Helper()
	ins := make([]Input, 0, len(reps))
	for i, name := range reps {
		o := fixture.Options{
			Seed: 42 + int64(i), DurationSeconds: 12, VideoTimescale: 90000, VideoFPS: 25,
			AudioTimescale: 48000, AudioDelta: 9600, KeyEvery: 25,
			StartDTS: 90000, EditList: true, VideoBody: 10, AudioBody: 6,
			VideoStreamID: fixture.StreamVideo() + uint32(i), AudioStreamID: fixture.StreamAudio() + uint32(i),
		}
		m, lay, err := fixture.GenerateFile(o)
		if err != nil {
			t.Fatal(err)
		}
		if damage != nil {
			damage(m, lay)
		}
		ins = append(ins, Input{
			ID: name + ".mp4", Representation: name,
			Reader: m.ReaderAt(), Size: int64(m.Len()),
		})
	}
	return ins
}

func rtpStats(t *testing.T, burst int) *rtp.Stats {
	t.Helper()
	s := rtp.NewStream(7, 90000, 3000)
	pkts := s.BuildPackets(100)
	cfg := rtp.LossConfig{BurstAt: 40, BurstLen: burst, ReorderAt: 5, DuplicateAt: 8}
	delivered := rtp.ApplyDamage(pkts, cfg)
	_, st, err := rtp.Reassemble(delivered, 100)
	if err != nil {
		t.Fatal(err)
	}
	return &st
}

func TestRunHealthyBatch(t *testing.T) {
	runner, err := NewRunner(t.TempDir(), Config{
		SegmentSeconds: 6, MaxDriftMillis: 40, CheckpointEvery: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	inputs := demoInputs(t, []string{"360p", "720p", "1080p"}, nil)
	m, manifest, err := runner.Run(context.Background(), inputs, rtpStats(t, 12))
	if err != nil {
		t.Fatal(err)
	}
	if m.AlignedRepresentations != 3 {
		t.Fatalf("aligned = %d, want 3", m.AlignedRepresentations)
	}
	if m.Segments == 0 {
		t.Fatal("no segments produced")
	}
	if m.AVSyncMaxMillis > 20 {
		t.Fatalf("av sync = %d ms, want <= 20", m.AVSyncMaxMillis)
	}
	if m.PacketRecoveryPct < 85 {
		t.Fatalf("recovery = %.2f, want >= 85", m.PacketRecoveryPct)
	}
	if m.ManifestSHA256 == "" {
		t.Fatal("missing manifest digest")
	}
	if len(manifest.QuarantinedRanges) != 0 {
		t.Fatal("healthy batch must not quarantine")
	}
}

func TestCancelThenResumeCompletes(t *testing.T) {
	dir := t.TempDir()
	inputs := demoInputs(t, []string{"360p", "720p"}, nil)

	first, err := NewRunner(dir, Config{
		SegmentSeconds: 6, MaxDriftMillis: 40, CheckpointEvery: 64,
		CancelAfterSamples: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = first.Run(context.Background(), inputs, nil)
	var me *model.MediaError
	if !errors.As(err, &me) || me.Code != model.CodeCanceled {
		t.Fatalf("expected cancellation error, got %v", err)
	}

	// Second run resumes from the checkpoint and must complete.
	second, err := NewRunner(dir, Config{
		SegmentSeconds: 6, MaxDriftMillis: 40, CheckpointEvery: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := second.Run(context.Background(), inputs, nil)
	if err != nil {
		t.Fatalf("resume run failed: %v", err)
	}
	if m.CheckpointResumes != 1 {
		t.Fatalf("resumes = %d, want 1", m.CheckpointResumes)
	}
}

func TestTruncationPropagatesToQuarantine(t *testing.T) {
	runner, err := NewRunner(t.TempDir(), Config{
		SegmentSeconds: 6, MaxDriftMillis: 40, CheckpointEvery: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	inputs := demoInputs(t, []string{"360p", "720p"}, func(m *fixture.MemFile, lay *fixture.Layout) {
		if err := fixture.TruncateTailFile(m, lay, 40); err != nil {
			t.Fatal(err)
		}
	})
	m, manifest, err := runner.Run(context.Background(), inputs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.QuarantinedRanges < 1 || len(manifest.QuarantinedRanges) < 1 {
		t.Fatal("truncated tail must produce at least one quarantined range")
	}
}

func TestReRunIsByteStableAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	inputs := demoInputs(t, []string{"360p", "720p", "1080p"}, nil)
	stats := rtpStats(t, 12)

	first, err := NewRunner(dir, Config{SegmentSeconds: 6, MaxDriftMillis: 40})
	if err != nil {
		t.Fatal(err)
	}
	m1, _, err := first.Run(context.Background(), inputs, stats)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRunner(dir, Config{SegmentSeconds: 6, MaxDriftMillis: 40})
	if err != nil {
		t.Fatal(err)
	}
	m2, _, err := second.Run(context.Background(), inputs, stats)
	if err != nil {
		t.Fatal(err)
	}
	if m1.ManifestSHA256 != m2.ManifestSHA256 {
		t.Fatal("fixed-seed reruns must produce identical manifest sha256")
	}
}

func TestForeignCheckpointRejected(t *testing.T) {
	dir := t.TempDir()
	inputsA := demoInputs(t, []string{"360p"}, nil)
	runner, err := NewRunner(dir, Config{SegmentSeconds: 6, MaxDriftMillis: 40})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.Run(context.Background(), inputsA, nil); err != nil {
		t.Fatal(err)
	}
	// Different seed -> different input identity -> resume must be refused.
	inputsB := demoInputs(t, []string{"720p"}, func(m *fixture.MemFile, lay *fixture.Layout) {
		_ = fixture.CorruptSampleFile(m, lay, 3)
	})
	_, _, err = runner.Run(context.Background(), inputsB, nil)
	var me *model.MediaError
	if !errors.As(err, &me) || me.Code != model.CodeCheckpointMismatch {
		t.Fatalf("expected checkpoint mismatch refusal, got %v", err)
	}
}
