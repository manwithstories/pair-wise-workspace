package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"mediacheck/internal/fixture"
	"mediacheck/internal/model"
	"mediacheck/internal/pipeline"
	"mediacheck/internal/rtp"
)

// executeDemo runs the full healthy/damaged/recovery demonstration and
// prints the eight acceptance fields as stable key=value lines.
func executeDemo(d DemoArgs) (int, error) {
	if err := os.MkdirAll(d.WorkDir, 0o755); err != nil {
		return exitIndexFailure, err
	}
	inputs, err := buildInputs(d)
	if err != nil {
		return exitUnrecoverable, err
	}

	// Local RTP loss/reorder/duplicate sample with pion/rtp reassembly.
	stream := rtp.NewStream(0x4D43, 90000, 3000)
	const rtpPackets = 100
	pkts := stream.BuildPackets(rtpPackets)
	cfg := rtp.LossConfig{
		BurstAt: 40, BurstLen: d.LossBurst, ReorderAt: 5, DuplicateAt: 8,
	}
	delivered := rtp.ApplyDamage(pkts, cfg)
	_, stats, err := rtp.Reassemble(delivered, rtpPackets)
	if err != nil {
		return exitUnrecoverable, err
	}

	indexDir := filepath.Join(d.WorkDir, "index")

	// First run deliberately cancels after a few checkpoints to prove the
	// resume path; the second run continues from the validated cursor.
	cancelRunner, err := pipeline.NewRunner(indexDir, pipeline.Config{
		SegmentSeconds:  d.SegmentSec,
		MaxDriftMillis:  40,
		CheckpointEvery: d.CheckpointEvery,
		CancelAfterSamples: d.CheckpointEvery * 2,
	})
	if err != nil {
		return exitBadArgs, err
	}
	if _, _, err := cancelRunner.Run(context.Background(), inputs, &stats); err != nil {
		var me *model.MediaError
		if !errors.As(err, &me) || me.Code != model.CodeCanceled {
			return mapRunError(err), err
		}
	}

	runner, err := pipeline.NewRunner(indexDir, pipeline.Config{
		SegmentSeconds:  d.SegmentSec,
		MaxDriftMillis:  40,
		CheckpointEvery: d.CheckpointEvery,
	})
	if err != nil {
		return exitBadArgs, err
	}

	metrics, _, err := runner.Run(context.Background(), inputs, &stats)
	if err != nil {
		return mapRunError(err), err
	}

	printMetrics(metrics)
	if err := assertAcceptance(metrics, d); err != nil {
		return exitUnrecoverable, err
	}
	return exitOK, nil
}

// buildInputs generates one MP4 per representation and injects the selected
// damage into the lowest-bitrate asset so every path is exercised locally.
func buildInputs(d DemoArgs) ([]pipeline.Input, error) {
	ins := make([]pipeline.Input, 0, len(d.Representations))
	for i, rep := range d.Representations {
		o := fixture.Options{
			Seed: d.Seed + int64(i), DurationSeconds: d.DurationSec,
			VideoTimescale: 90000, VideoFPS: demoVideoFPS,
			AudioTimescale: 48000, AudioDelta: demoAudioDelta,
			KeyEvery: demoVideoFPS * uint32(d.SegmentSec),
			StartDTS: 90000, EditList: true,
			VideoBody: videoBodyFor(rep), AudioBody: 8,
			VideoStreamID: fixture.StreamVideo() + uint32(i),
			AudioStreamID: fixture.StreamAudio() + uint32(i),
		}
		m, lay, err := fixture.GenerateFile(o)
		if err != nil {
			return nil, err
		}
		// Damage the first representation only; others stay healthy.
		if i == 0 && d.TruncateTail == 1 {
			drop := int64(40)
			if drop >= lay.TotalSize {
				drop = lay.TotalSize / 2
			}
			if err := fixture.TruncateTailFile(m, lay, drop); err != nil {
				return nil, err
			}
		}
		ins = append(ins, pipeline.Input{
			ID: rep + ".mp4", Representation: rep,
			Reader: m.ReaderAt(), Size: int64(m.Len()),
			Truncated: i == 0 && d.TruncateTail == 1,
		})
	}
	return ins, nil
}

const (
	demoVideoFPS   uint32 = 2
	demoAudioDelta uint32 = 9600 // 5 samples/sec at 48 kHz
)

func videoBodyFor(rep string) int {
	switch rep {
	case "360p":
		return 64
	case "720p":
		return 128
	case "1080p":
		return 256
	default:
		return 96
	}
}

func mapRunError(err error) int {
	var me *model.MediaError
	if errors.As(err, &me) {
		switch me.Code {
		case model.CodeIndexWriteFailed, model.CodeCheckpointMismatch:
			return exitIndexFailure
		default:
			return exitUnrecoverable
		}
	}
	return exitUnrecoverable
}

func printMetrics(m *pipeline.Metrics) {
	fmt.Printf("av_sync_max_ms=%d\n", m.AVSyncMaxMillis)
	fmt.Printf("packet_recovery_pct=%.2f\n", m.PacketRecoveryPct)
	fmt.Printf("p95_segment_latency_ms=%d\n", m.P95SegmentLatencyMs)
	fmt.Printf("segments=%d\n", m.Segments)
	fmt.Printf("aligned_representations=%d\n", m.AlignedRepresentations)
	fmt.Printf("checkpoint_resumes=%d\n", m.CheckpointResumes)
	fmt.Printf("quarantined_ranges=%d\n", m.QuarantinedRanges)
	fmt.Printf("manifest_sha256=%s\n", m.ManifestSHA256)
}

// assertAcceptance enforces the published numeric gates. A failure here is a
// real media/processing error (exit 3), not a CLI argument problem.
func assertAcceptance(m *pipeline.Metrics, d DemoArgs) error {
	if m.AVSyncMaxMillis > 20 {
		return fmt.Errorf("av_sync_max_ms %d exceeds 20", m.AVSyncMaxMillis)
	}
	if m.AlignedRepresentations != len(d.Representations) {
		return fmt.Errorf("aligned_representations %d != %d",
			m.AlignedRepresentations, len(d.Representations))
	}
	if m.PacketRecoveryPct < 85 {
		return fmt.Errorf("packet_recovery_pct %.2f below 85", m.PacketRecoveryPct)
	}
	if m.P95SegmentLatencyMs > 500 {
		return fmt.Errorf("p95_segment_latency_ms %d exceeds 500", m.P95SegmentLatencyMs)
	}
	if d.TruncateTail == 1 && m.QuarantinedRanges < 1 {
		return errors.New("truncated tail produced no quarantined range")
	}
	return nil
}
