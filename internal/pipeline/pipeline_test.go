package pipeline_test

import (
	"context"
	"testing"

	"mediacheck/internal/fixture"
	"mediacheck/internal/pipeline"
)

func testOptions(dir string) pipeline.Options {
	return pipeline.Options{
		OutDir: dir, Resume: true, TargetBPS: fixture.ContainerTargetBPS,
		VideoTargetBPS: fixture.VideoTargetBPS, QueueItems: 8, QueueBytes: 32768,
	}
}

func testInput(b *fixture.Bundle) (pipeline.VideoInput, pipeline.AudioInput) {
	return pipeline.VideoInput{
		Codec: b.Codec, Data: b.Video, Clock: *b.Clock, FrameDuration: b.FrameDur,
	}, pipeline.AudioInput{Data: b.PCM, Format: b.PCMFormat}
}

func TestRunNormalMetrics(t *testing.T) {
	b := fixture.Normal()
	video, audio := testInput(b)
	res, err := pipeline.Run(context.Background(), testOptions(t.TempDir()), video, audio)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Report.DeviationPct > 5 || res.Report.DeviationPct < -5 {
		t.Fatalf("deviation=%.3f", res.Report.DeviationPct)
	}
	if d := res.Report.AudioLUFS + 23; d > 1 || d < -1 {
		t.Fatalf("lufs=%.3f", res.Report.AudioLUFS)
	}
	if res.Report.TruePeakDBFS > -1 {
		t.Fatalf("peak=%.3f", res.Report.TruePeakDBFS)
	}
	if res.Report.ManifestSkewUS > 1000 || res.Report.FrameCount != fixture.FrameCount {
		t.Fatalf("skew=%d frames=%d", res.Report.ManifestSkewUS, res.Report.FrameCount)
	}
}

func TestFreshRunsHaveIdenticalHash(t *testing.T) {
	b := fixture.Normal()
	video, audio := testInput(b)
	first, err := pipeline.Run(context.Background(), testOptions(t.TempDir()), video, audio)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pipeline.Run(context.Background(), testOptions(t.TempDir()), video, audio)
	if err != nil {
		t.Fatal(err)
	}
	if first.Report.SHA256 != second.Report.SHA256 {
		t.Fatalf("hashes differ: %s != %s", first.Report.SHA256, second.Report.SHA256)
	}
}
