// Package pipeline wires ingestion, bounded queues, audio normalization,
// fMP4 muxing, WAL transactions and manifest/metrics verification.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"mediacheck/internal/audio"
	"mediacheck/internal/ingest"
	"mediacheck/internal/manifest"
	"mediacheck/internal/media"
	"mediacheck/internal/metrics"
	"mediacheck/internal/mux"
	"mediacheck/internal/txn"
)

const (
	StageIngest   = "ingest"
	StageIndex    = "keyindex"
	StageAudio    = "audio"
	StageMux      = "mux"
	StageManifest = "manifest"
	StageReport   = "report"

	TxID = "batch-quality-job"
)

// VideoInput is raw elementary video and timing parameters.
type VideoInput struct {
	Codec         ingest.Codec
	Data          []byte
	Clock         media.ClockDeclaration
	FrameDuration int64
	Index         *ingest.KeyframeIndex
}

// AudioInput is interleaved s16le PCM and its source format.
type AudioInput struct {
	Data   []byte
	Format ingest.PCMFormat
}

// Result contains verified artifacts and reporting data.
type Result struct {
	Units           []ingest.Unit
	NormalizedPCM   []byte
	Audio           audio.LoudnessResult
	MuxResult       *mux.Result
	Report          metrics.Report
	Manifest        manifest.Manifest
	RecoveredStages []string
	WAL             *txn.WAL
}

// Run executes or resumes one quality-verification job.
func Run(ctx context.Context, opts Options, video VideoInput, pcm AudioInput) (*Result, error) {
	if opts.TargetBPS <= 0 {
		opts.TargetBPS = 2_550_000
	}
	if opts.VideoTargetBPS <= 0 {
		opts.VideoTargetBPS = 1_000_000
	}
	if opts.MemoryLimitBytes > 0 {
		// debug.SetMemoryLimit is applied by main; option documents job budget.
		_ = opts.MemoryLimitBytes
	}

	units, err := ingest.ParseElementary(ingest.ParseRequest{
		Codec: video.Codec, Data: video.Data, Clock: &video.Clock,
		FrameDuration: video.FrameDuration, MaxBytes: 8 << 20,
	})
	if err != nil {
		return nil, err
	}
	vcl := vclUnits(units, video.Codec)
	if len(vcl) == 0 {
		return nil, media.E(media.StageIngest, media.KindValidation, 0,
			fmt.Errorf("no VCL video frames found"))
	}
	videoTicks := int64(len(vcl)) * video.FrameDuration

	pcmStream, err := ingest.ReadS16LE(pcm.Data, pcm.Format, &video.Clock, 8<<20)
	if err != nil {
		return nil, err
	}
	rawAudio := audio.FromS16(pcmStream.Data, pcm.Format.SampleRate, pcm.Format.Channels)
	converted := audio.ConvertFormat(rawAudio, audio.TargetRate, audio.TargetChannels)
	clipped := audio.CountClippedS16(pcm.Data)
	loud := audio.Normalize(converted, -23, -1, clipped)
	normalizedPCM := loud.Buffer.ToS16()
	audioTicks := media.PCMTicks(int64(loud.Buffer.Frames()), audio.TargetRate)
	audioFragment := buildAudioFragment(normalizedPCM)

	segments, err := routeVideoThroughQueue(ctx, opts, vcl, video.Codec, video.FrameDuration)
	if err != nil {
		return nil, err
	}
	built, err := mux.BuildFMP4(mux.Config{
		DurationTicks: uint32(videoTicks), Width: 320, Height: 180, AudioChannels: audio.TargetChannels,
	}, segments, &audioFragment)
	if err != nil {
		return nil, err
	}
	segmentReports, err := metrics.CheckSegments(opts.VideoTargetBPS, built.Segments)
	if err != nil {
		return nil, err
	}
	actualBPS, deviation := metrics.Overall(opts.TargetBPS, int64(len(built.Bytes)), videoTicks)
	if abs64(deviation) > metrics.BitrateTolerancePercent {
		return nil, media.E(media.StageMetrics, media.KindValidation, 0,
			fmt.Errorf("overall bitrate deviation %.2f%% exceeds %.1f%%",
				deviation, metrics.BitrateTolerancePercent))
	}

	keyCount := countKeyframes(vcl)
	indexEntries := keyCount
	indexTicks := videoTicks
	var indexJSON []byte
	if video.Index != nil {
		indexTicks, err = crossCheckKeyIndex(vcl, video.Index)
		if err != nil {
			return nil, err
		}
		indexEntries = len(video.Index.Entries)
		indexJSON, err = json.MarshalIndent(video.Index, "", "  ")
		if err != nil {
			return nil, media.E(media.StageIndex, media.KindInvalidArgument, -1, err)
		}
	}
	alignment, err := manifest.AlignDurations(videoTicks, audioTicks, indexTicks)
	if err != nil {
		return nil, err
	}
	if err := alignment.CheckCounts(len(vcl), loud.Buffer.Frames(), indexEntries, keyCount); err != nil {
		return nil, err
	}

	recovery, err := txn.Recover(opts.OutDir, TxID)
	if err != nil {
		return nil, err
	}
	wal, err := txn.Open(opts.OutDir, TxID)
	if err != nil {
		return nil, err
	}
	res := &Result{Units: units, NormalizedPCM: normalizedPCM, Audio: loud, MuxResult: built, WAL: wal}
	if opts.Resume {
		res.RecoveredStages = recovery.Stages(StageIngest, StageIndex, StageAudio, StageMux, StageManifest, StageReport)
	}

	ingestMeta := map[string]any{
		"codec": string(video.Codec), "frame_count": len(vcl),
		"keyframe_count": keyCount, "source_sha256": txn.SHA256Bytes(video.Data),
	}
	ingestJSON, _ := json.MarshalIndent(ingestMeta, "", "  ")
	if _, err := commitBytes(recovery, wal, StageIngest, "ingest.json", ingestJSON, videoTicks); err != nil {
		return res, err
	}
	if video.Index != nil {
		if _, err := commitBytes(recovery, wal, StageIndex, "keyindex.json", indexJSON, indexTicks); err != nil {
			return res, err
		}
	}
	if _, err := commitBytes(recovery, wal, StageAudio, "audio.s16le", normalizedPCM, audioTicks); err != nil {
		return res, err
	}
	muxRec, err := commitBytes(recovery, wal, StageMux, "fmp4.mp4", built.Bytes, videoTicks)
	if err != nil {
		return res, err
	}
	if opts.CrashAfterMux {
		_ = wal.Close()
		return res, media.E(media.StageMux, media.KindInterrupted, 0,
			fmt.Errorf("injected crash after committed fMP4 stage sha256=%s", muxRec.SHA256[:12])).
			WithRetryable()
	}

	mf := manifest.Manifest{
		TxID: TxID, Codec: string(video.Codec), Clock: video.Clock, OutputRel: "fmp4.mp4",
		SHA256: txn.SHA256Bytes(built.Bytes), FrameCount: len(vcl), KeyframeCount: keyCount,
		TargetBPS: opts.TargetBPS, ActualBPS: actualBPS,
		AudioSampleRate: audio.TargetRate, AudioChannels: audio.TargetChannels,
		AudioLUFS: loud.LUFS, TruePeakDBFS: loud.TruePeakDBFS, Alignment: alignment,
		RecoveredStages: res.RecoveredStages,
	}
	manifestJSON, _ := json.MarshalIndent(mf, "", "  ")
	if _, err := commitBytes(recovery, wal, StageManifest, "manifest.json", manifestJSON, videoTicks); err != nil {
		return res, err
	}
	report := metrics.Report{
		TargetBPS: opts.TargetBPS, ActualBPS: actualBPS, DeviationPct: deviation,
		AudioLUFS: loud.LUFS, TruePeakDBFS: loud.TruePeakDBFS,
		ManifestSkewUS: alignment.SkewMicroseconds, FrameCount: len(vcl),
		SHA256: mf.SHA256, Segments: segmentReports,
	}
	reportJSON, _ := json.MarshalIndent(report, "", "  ")
	if _, err := commitBytes(recovery, wal, StageReport, "report.json", reportJSON, videoTicks); err != nil {
		return res, err
	}
	res.Report = report
	res.Manifest = mf
	return res, wal.Close()
}

func commitBytes(rec *txn.Recovery, wal *txn.WAL, stage, rel string, data []byte, duration int64) (txn.Record, error) {
	hash := txn.SHA256Bytes(data)
	if rec != nil && rec.Has(stage) {
		if old, ok := rec.Record(stage); ok && old.SHA256 == hash && old.Size == int64(len(data)) {
			return old, nil
		}
	}
	p, err := wal.Begin(stage, rel, duration)
	if err != nil {
		return txn.Record{}, err
	}
	if err := p.Write(data); err != nil {
		return txn.Record{}, err
	}
	if err := p.Verify(func(rec txn.Record) error {
		if rec.SHA256 != hash || rec.Size != int64(len(data)) {
			return media.E(media.StageWAL, media.KindHashMismatch, rec.Offset,
				fmt.Errorf("stage %s hash/size verification failed", stage))
		}
		if duration > 0 && rec.DurationTicks != duration {
			return media.E(media.StageWAL, media.KindValidation, rec.Offset,
				fmt.Errorf("stage %s duration=%d want %d", stage, rec.DurationTicks, duration))
		}
		return nil
	}); err != nil {
		return txn.Record{}, err
	}
	return p.Commit()
}

func vclUnits(units []ingest.Unit, codec ingest.Codec) []ingest.Unit {
	var out []ingest.Unit
	for _, u := range units {
		if u.Duration > 0 {
			out = append(out, u)
		}
	}
	return out
}

func countKeyframes(units []ingest.Unit) int {
	n := 0
	for _, u := range units {
		if u.Keyframe {
			n++
		}
	}
	return n
}

func abs64(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
