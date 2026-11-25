package demo

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/pion/rtp"

	"mediacheck/internal/fixture"
	"mediacheck/internal/ingest"
	"mediacheck/internal/media"
	"mediacheck/internal/pipeline"
	"mediacheck/internal/txn"
)

// Config configures the terminal demonstration.
type Config struct {
	Out    string
	Faults map[string]bool
	Resume bool
}

// Run executes the normal sample, expected fault paths and crash recovery.
func Run(cfg Config) error {
	if cfg.Out == "" {
		return media.E(media.StageDemo, media.KindInvalidArgument, -1,
			fmt.Errorf("--out is required"))
	}
	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return media.E(media.StageDemo, media.KindResource, -1, err)
	}
	bundle := fixture.Normal()
	if err := validateNormalIndex(bundle); err != nil {
		return err
	}

	normalDir, err := txn.SafeJoin(cfg.Out, "normal")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(normalDir, 0o755); err != nil {
		return media.E(media.StageDemo, media.KindResource, -1, err)
	}
	opts := pipeline.Options{
		OutDir: normalDir, Resume: cfg.Resume, TargetBPS: fixture.ContainerTargetBPS,
		VideoTargetBPS: fixture.VideoTargetBPS, QueueItems: 8, QueueBytes: 32768,
		MemoryLimitBytes: 32 << 20,
	}
	res, err := pipeline.Run(context.Background(), opts, pipeline.VideoInput{
		Codec: bundle.Codec, Data: bundle.Video, Clock: *bundle.Clock,
		FrameDuration: bundle.FrameDur, Index: bundle.Index,
	}, pipeline.AudioInput{Data: bundle.PCM, Format: bundle.PCMFormat})
	if err != nil {
		return err
	}
	printNormalReport(res)

	lines, err := CheckFaults(cfg.Faults)
	for _, line := range lines {
		fmt.Println(line)
	}
	if err != nil {
		return err
	}
	if cfg.Faults["crash-after-mux"] {
		return runCrashRecovery(cfg)
	}
	return nil
}

func validateNormalIndex(b *fixture.Bundle) error {
	if err := b.Index.ValidateAgainstSource(b.Video, b.Index.SourceSHA256); err != nil {
		return err
	}
	decoded := make([]rtp.Packet, 0, len(b.RTPPackets))
	for _, pkt := range b.RTPPackets {
		raw, err := pkt.Marshal()
		if err != nil {
			return media.E(media.StageIndex, media.KindValidation, -1, err)
		}
		p, err := ingest.DecodeRTPPacket(raw)
		if err != nil {
			return err
		}
		decoded = append(decoded, p)
	}
	return b.Index.ValidateRTP(decoded)
}

func printNormalReport(res *pipeline.Result) {
	r := res.Report
	fmt.Printf("sample=normal status=verified target_bps=%d actual_bps=%d deviation_pct=%.3f audio_lufs=%.2f true_peak_dbfs=%.2f manifest_skew_us=%d frame_count=%d sha256=%s video_reencoded=false\n",
		r.TargetBPS, r.ActualBPS, r.DeviationPct, r.AudioLUFS, r.TruePeakDBFS,
		r.ManifestSkewUS, r.FrameCount, r.SHA256)
	for _, s := range r.Segments {
		fmt.Printf("segment=%d base_pts=%d duration_ticks=%d bytes=%d target_bps=%d actual_bps=%d deviation_pct=%.3f within_tolerance=%t\n",
			s.Index, s.BasePTS, s.DurationTicks, s.Bytes, s.TargetBPS, s.ActualBPS,
			s.DeviationPct, s.WithinTolerance)
	}
}

func runCrashRecovery(cfg Config) error {
	crashDir, err := txn.SafeJoin(cfg.Out, "crash")
	if err != nil {
		return err
	}
	if err := os.RemoveAll(crashDir); err != nil {
		return media.E(media.StageDemo, media.KindResource, -1, err)
	}
	if err := os.MkdirAll(crashDir, 0o755); err != nil {
		return media.E(media.StageDemo, media.KindResource, -1, err)
	}
	b := fixture.Normal()
	opts := pipeline.Options{
		OutDir: crashDir, TargetBPS: fixture.ContainerTargetBPS,
		VideoTargetBPS: fixture.VideoTargetBPS, QueueItems: 8, QueueBytes: 32768,
	}
	in := pipeline.VideoInput{Codec: b.Codec, Data: b.Video, Clock: *b.Clock,
		FrameDuration: b.FrameDur, Index: b.Index}
	pcm := pipeline.AudioInput{Data: b.PCM, Format: b.PCMFormat}

	first, crashErr := pipeline.Run(context.Background(), withCrash(opts, false, true), in, pcm)
	code := media.ExitCode(crashErr)
	if code != 75 {
		return media.E(media.StageDemo, media.KindValidation, -1,
			fmt.Errorf("crash injection exit=%d err=%v", code, crashErr))
	}
	me, _ := media.AsError(crashErr)
	fmt.Printf("fault=crash-after-mux status=injected_interrupt error_stage=%s error_offset=%d exit_code=75 retryable=true\n",
		me.Stage, me.Offset)
	if !cfg.Resume {
		return crashErr
	}
	if first != nil && first.WAL != nil {
		_ = first.WAL.Close()
	}

	second, err := pipeline.Run(context.Background(), withCrash(opts, true, false), in, pcm)
	if err != nil {
		return err
	}
	recovery, err := txn.Recover(crashDir, pipeline.TxID)
	if err != nil {
		return err
	}
	muxRec, ok := recovery.Record(pipeline.StageMux)
	if !ok {
		return media.E(media.StageRecover, media.KindValidation, -1,
			fmt.Errorf("committed mux stage missing after resume"))
	}
	muxPath, err := txn.SafeJoin(crashDir, "fmp4.mp4")
	if err != nil {
		return err
	}
	fileHash, _, err := txn.HashFile(muxPath)
	if err != nil {
		return media.E(media.StageRecover, media.KindResource, 0, err)
	}
	hashMatch := muxRec.SHA256 == second.Report.SHA256 && fileHash == second.Report.SHA256

	third, err := pipeline.Run(context.Background(), withCrash(opts, true, false), in, pcm)
	if err != nil {
		return err
	}
	rerunMatch := third.Report.SHA256 == second.Report.SHA256
	fmt.Printf("recovery status=resumed wal_recovered_stages=%s output_sha256=%s wal_mux_sha256=%s file_sha256=%s hash_match=%t rerun_hash_match=%t\n",
		strings.Join(second.RecoveredStages, ","), second.Report.SHA256, muxRec.SHA256,
		fileHash, hashMatch, rerunMatch)
	if !hashMatch || !rerunMatch {
		return media.E(media.StageRecover, media.KindHashMismatch, 0,
			fmt.Errorf("recovery hash consistency check failed"))
	}
	return nil
}

func withCrash(opts pipeline.Options, resume, crash bool) pipeline.Options {
	opts.Resume = resume
	opts.CrashAfterMux = crash
	return opts
}
