// Package pipeline wires every subsystem into the end-to-end data flow:
// scan/index -> transport recovery & clock normalization -> metadata merge and
// cut/silence/fingerprint analysis -> remux and metrics.
package pipeline

import (
	"context"

	"mediaflow/internal/clock"
	"mediaflow/internal/fixture"
	"mediaflow/internal/index"
	"mediaflow/internal/metadata"
	"mediaflow/internal/quality"
	"mediaflow/internal/remux"
	"mediaflow/internal/report"
	"mediaflow/internal/stream"
	"mediaflow/internal/transport"
)

// Case names accepted by the demo command.
const (
	CaseClean  = "clean"
	CaseDamage = "damage"
	CaseResume = "resume"
)

// Outcome bundles everything a case produced.
type Outcome struct {
	Metrics        report.Metrics
	Index          *index.Index
	Normalized     []clock.Normalized
	MetaReport     *metadata.Report
	TransportStats transport.Stats
	Gaps           []transport.Gap
	StreamStats    stream.RunStats
	CutPoints      []quality.CutPoint
	Segments       []quality.Segment
	Silence        []quality.SilenceRange
	Duplicates     []quality.Match
}

// Config controls one end-to-end run.
type Config struct {
	Seed          int64
	Root          string
	ReorderWindow int
}

// damageSpecForCase maps a demo case to its deterministic damage profile.
func damageSpecForCase(name string) fixture.DamageSpec {
	switch name {
	case CaseDamage:
		// Recoverable single losses plus deliberately unrecovered multi-loss.
		return fixture.DamageSpec{
			DropSingles:    []int{0, 2, 4, 6, 8},
			DropMultiples:  map[int]int{10: 3},
			DuplicateSeq:   map[uint32][]uint32{1: {3, 7}, 2: {5}},
			ReorderWindow:  fixture.FECGroupSize,
			TruncateEvents: []int{1, 9},
		}
	case CaseResume:
		// Recoverable-only damage so the resumed output hash still matches.
		return fixture.DamageSpec{
			DropSingles:    []int{1, 3, 5, 7},
			DuplicateSeq:   map[uint32][]uint32{1: {11}},
			ReorderWindow:  fixture.FECGroupSize,
			TruncateEvents: nil,
		}
	default: // clean
		return fixture.DamageSpec{ReorderWindow: fixture.FECGroupSize}
	}
}

// Run executes one named case end to end.
func Run(ctx context.Context, name string, cfg Config) (*Outcome, error) {
	// 1. Deterministic fixture generation and input digest.
	base, err := fixture.Generate(fixture.DefaultConfig(cfg.Seed))
	if err != nil {
		return nil, err
	}
	base.Spec = damageSpecForCase(name)
	clip := fixture.ApplyDamage(base)
	inputSHA, err := index.HashClip(clip)
	if err != nil {
		return nil, err
	}

	// 2. Scan/index with version + digest validated cache.
	cache, err := index.NewCache(cfg.Root + "/index")
	if err != nil {
		return nil, err
	}
	idx, _, err := cache.GetOrScan(clip, inputSHA)
	if err != nil {
		return nil, err
	}

	// 3. Transport recovery: reorder, dedup, FEC, precise gaps. Per-track
	// timestamp wrap moduli from the fixture drive timestamp unwrapping.
	tsMod := map[uint32]int64{}
	for _, tr := range clip.Tracks {
		if tr.WrapModulus > 0 {
			tsMod[tr.ID] = tr.WrapModulus
		}
	}
	recv, err := transport.NewReceiver(transport.Config{
		ReorderWindow: cfg.ReorderWindow, TSWrapModulus: tsMod,
	})
	if err != nil {
		return nil, err
	}
	delivery := fixture.ExpandDuplicates(clip.Packets)
	for _, p := range delivery {
		if err := recv.Ingest(p); err != nil {
			return nil, err
		}
	}
	recovered, gaps, tstats := recv.Flush()

	// 4. Clock normalization and drift correction.
	normalizer := clock.NewNormalizer()
	if err := normalizer.Estimate(recovered, clip.Tracks); err != nil {
		return nil, err
	}
	normalized, err := normalizer.NormalizeAll(recovered)
	if err != nil {
		return nil, err
	}
	maxRes, _ := normalizer.MaxResidual(recovered)

	// 5. Cross-track metadata merge with truncation isolation.
	meta, err := metadata.Merge(clip.Events, idx.SpanUS, metadata.DefaultConfig())
	if err != nil {
		return nil, err
	}

	// 6. Quality analysis on the normalized timeline.
	videoFrames := filterTrack(normalized, 1)
	cutCfg := quality.DefaultCutConfig()
	for _, ch := range meta.FindByKind(fixture.EventChapter) {
		cutCfg.HintUS = append(cutCfg.HintUS, ch.StartUS)
	}
	cutPoints, segments, err := quality.DetectCuts(videoFrames, cutCfg)
	if err != nil {
		return nil, err
	}
	audioFrames := toAudioFrames(filterTrack(normalized, 2))
	silence, err := quality.DetectSilence(audioFrames, quality.DefaultSilenceConfig())
	if err != nil {
		return nil, err
	}
	duplicates := quality.FindDuplicates(videoFrames, quality.DefaultFingerprintConfig())

	// 7. Streaming remux with checkpoints and atomic output.
	w, err := remux.NewWriter(cfg.Root + "/out")
	if err != nil {
		return nil, err
	}
	cp, err := stream.NewCheckpointer(cfg.Root+"/checkpoint", name+"-checkpoint.json")
	if err != nil {
		return nil, err
	}
	runner := stream.NewRunner(w, cp, stream.RunConfig{
		OutputName: name + ".bin", MaxFramesPerSegment: 64,
	})
	sstats, err := runner.Run(ctx, normalized, inputSHA)
	if err != nil {
		return nil, err
	}

	// 8. Output hash must match a fresh deterministic encoding of the frames.
	wantBytes, _ := remux.EncodeFrames(normalized)
	wantSum := quality.HashPayload(wantBytes)
	outputMatch := sstats.OutputSHA == hexSum(wantSum[:])

	m := report.Build(report.Inputs{
		Case: name, SyncResidualUS: maxRes, Transport: tstats,
		P95SegmentUS: sstats.P95US, IndexHash: idx.ContentHash(),
		OutputShaMatch: outputMatch, TruncatedEvents: len(meta.Truncated),
	})
	return &Outcome{
		Metrics: m, Index: idx, Normalized: normalized, MetaReport: meta,
		TransportStats: tstats, Gaps: gaps, StreamStats: sstats,
		CutPoints: cutPoints, Segments: segments,
		Silence: silence, Duplicates: duplicates,
	}, nil
}

func filterTrack(frames []clock.Normalized, track uint32) []clock.Normalized {
	var out []clock.Normalized
	for _, f := range frames {
		if f.Track == track {
			out = append(out, f)
		}
	}
	return out
}

// toAudioFrames wraps normalized audio frames with deterministic synthetic PCM
// derived from their payload, keeping the demo dependency-free. Frames marked
// silent produce all-zero samples so RMS silence detection has real input.
func toAudioFrames(frames []clock.Normalized) []quality.AudioFrame {
	out := make([]quality.AudioFrame, 0, len(frames))
	for _, f := range frames {
		samples := make([]float64, 32)
		if !f.Silent {
			for i := range samples {
				b := byte(0)
				if i < len(f.Payload) {
					b = f.Payload[i]
				}
				samples[i] = float64(b)/255.0*2 - 1
			}
		}
		out = append(out, quality.AudioFrame{Frame: f, Samples: samples})
	}
	return out
}
