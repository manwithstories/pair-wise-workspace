package pipeline

import (
	"errors"
	"os"
	"path/filepath"

	"mediapipe/internal/audio"
	"mediapipe/internal/cache"
	"mediapipe/internal/checkpoint"
	"mediapipe/internal/cut"
	"mediapipe/internal/encode"
	"mediapipe/internal/fingerprint"
	"mediapipe/internal/manifest"
	"mediapipe/internal/media"
	"mediapipe/internal/source"
	"mediapipe/internal/transport"
)

type Options struct {
	Work       string
	Seed       int64
	Seconds    int
	TargetKbps int
}

type Result struct {
	Manifest *manifest.Manifest
	Cache    cache.Verification
}

// RunDemo executes the incremental broadcast pipeline. It is deterministic for
// a fixed seed and safe to invoke again against the same work directory.
func RunDemo(opt Options) (*Result, error) {
	if opt.Work == "" {
		opt.Work = filepath.Join(".tmp", "demo")
	}
	if opt.Seconds <= 0 {
		opt.Seconds = 30
	}
	if opt.TargetKbps <= 0 {
		opt.TargetKbps = 1200
	}
	limits := media.DefaultLimits().Normalize()
	if err := os.MkdirAll(filepath.Join(opt.Work, "segments"), 0o755); err != nil {
		return nil, media.WrapError(media.CodeIO, "create work directories", err)
	}

	sourcePath, err := media.SafeJoin(opt.Work, "source.mp4")
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(sourcePath); errors.Is(err, os.ErrNotExist) {
		if err := source.GenerateSyntheticMP4(source.SyntheticConfig{
			Path: sourcePath, Seconds: opt.Seconds, Width: 320, Height: 180,
			FPS: 15, BitrateKbps: 500, Seed: opt.Seed,
		}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, media.WrapError(media.CodeIO, "stat source fixture", err)
	}
	st, err := os.Stat(sourcePath)
	if err != nil {
		return nil, media.WrapError(media.CodeIO, "stat generated source", err)
	}
	if err := limits.CheckInputSize(st.Size()); err != nil {
		return nil, err
	}
	sourceSHA, err := media.FileSHA256(sourcePath)
	if err != nil {
		return nil, err
	}
	demux, err := source.Demux(sourcePath)
	if err != nil {
		return nil, err
	}

	ordered, sim, rb, err := restoreTransport(opt.Seed, demux.Packets)
	if err != nil {
		return nil, err
	}
	cutRes, err := cut.PlanSegments(ordered, cut.Options{
		TargetSeconds: demoSegmentSeconds, FrameRate: media.NewRat(15, 1),
		VideoStream: firstVideoStream(demux.Info), AudioStream: 1000,
	})
	if err != nil {
		return nil, err
	}
	audioReport, sigs, duplicates, err := analyzeContent(opt, cutRes.Segments)
	if err != nil {
		return nil, err
	}

	cfg := encode.DefaultConfig(opt.TargetKbps, opt.Seconds)
	configSHA, err := cfg.SHA256()
	if err != nil {
		return nil, media.WrapError(media.CodeInvalidInput, "encode config digest", err)
	}
	store, err := cache.Open(filepath.Join(opt.Work, "cache"), limits.MaxCacheEntries, 64<<20)
	if err != nil {
		return nil, err
	}
	journalPath, err := media.SafeJoin(opt.Work, "checkpoint.json")
	if err != nil {
		return nil, err
	}
	journal, err := checkpoint.Open(journalPath, "mediapipe-demo", sourceSHA, configSHA)
	if err != nil {
		return nil, err
	}

	mf := manifest.New("mediapipe-demo", opt.Seed, sourceSHA)
	mf.Silence = make([]manifest.SilenceRegion, 0, len(audioReport.Regions))
	for _, r := range audioReport.Regions {
		mf.Silence = append(mf.Silence, manifest.SilenceRegion{
			StartSec: r.StartSec, EndSec: r.EndSec, DurationSec: r.DurationSec, Boundary: r.Boundary,
		})
	}
	var totalBytes int64
	var verification cache.Verification
	snap := journal.Snapshot()
	for _, sp := range cutRes.Segments {
		finalPath, err := media.SafeJoin(opt.Work, filepath.Join("segments", sp.Output))
		if err != nil {
			return nil, err
		}
		var art checkpoint.Artifact
		if old, ok := snap.Artifacts[sp.Output]; ok {
			if sum, statErr := media.FileSHA256(finalPath); statErr == nil && sum == old.SHA256 {
				art = old
			}
		}
		if art.SHA256 == "" {
			duration := sp.TargetEndSec - sp.TargetStartSec
			if duration <= 0 {
				duration = demoSegmentSeconds
			}
			budget := encode.NewController(cfg, duration).TargetBytes
			n, sum, err := encode.RemuxSegmentAtomic(sourcePath, finalPath, sp.TargetStartSec, sp.TargetEndSec, budget)
			if err != nil {
				return nil, err
			}
			art, err = journal.RecordProduct(sp.Output, finalPath, sp.EndSequence)
			if err != nil {
				return nil, err
			}
			art.SizeBytes, art.SHA256 = n, sum
		}
		data, err := os.ReadFile(finalPath)
		if err != nil {
			return nil, media.WrapError(media.CodeIO, "read segment for cache", err)
		}
		key, err := cache.Key(sourceSHA, map[string]any{
			"config": configSHA, "segment": sp.Index, "start": sp.TargetStartSec, "end": sp.TargetEndSec,
		})
		if err != nil {
			return nil, err
		}
		if _, err := store.Put(key, sourceSHA, configSHA, sp.Output, data); err != nil {
			return nil, err
		}
		verification, err = store.VerifyHit(key, sourcePath)
		if err != nil {
			return nil, err
		}
		if !verification.Hit || !verification.SourceOK || !verification.SHA256OK {
			return nil, media.NewError(media.CodeChecksumMismatch, "fresh cache entry failed verification", nil)
		}
		totalBytes += art.SizeBytes
		mf.AddSegment(manifest.Segment{
			Index: sp.Index, Name: sp.Output, Path: filepath.Join("segments", sp.Output),
			SHA256: art.SHA256, SizeBytes: art.SizeBytes,
			TargetStartSec: sp.TargetStartSec, ActualStartSec: sp.ActualStartSec,
			TargetEndSec: sp.TargetEndSec, ActualEndSec: sp.ActualEndSec,
			AlignmentErrorMS: sp.AudioVideoErrorMS,
		})
	}

	mf.Stats.DecodeErrors = demux.DecodeErrors
	mf.Stats.AudioPeakDBFS = audioReport.PeakDBFS
	mf.Stats.SilenceRegions = len(audioReport.Regions)
	mf.Stats.DuplicateSegments = duplicates
	mf.SetBitrate(totalBytes, float64(opt.Seconds), opt.TargetKbps)
	mf.Recovery = &manifest.Recovery{
		DuplicatePackets: rb.Stats.DuplicatePackets, ReorderedPackets: sim.Stats.ReorderedPackets,
		RecoveredPackets: rb.Stats.RecoveredPackets, LostPackets: rb.Stats.LostPackets,
		CacheHit: verification.Hit, SourceOK: verification.SourceOK, SHA256OK: verification.SHA256OK,
		CheckpointSeq: journal.Snapshot().NextSeq,
	}
	if err := mf.ValidateAlignment(42); err != nil {
		return nil, err
	}
	manifestPath, err := media.SafeJoin(opt.Work, "manifest.json")
	if err != nil {
		return nil, err
	}
	if _, _, err := manifest.WriteAtomicJSON(manifestPath, mf); err != nil {
		return nil, err
	}
	_ = sigs
	return &Result{Manifest: &mf, Cache: verification}, nil
}

func restoreTransport(seed int64, in []media.Packet) ([]media.Packet, *transport.Simulator, *transport.ReorderBuffer, error) {
	cfg := transport.DefaultConfig(seed, len(in))
	sim := transport.NewSimulator(cfg, len(in))
	arrivals := sim.Transmit(in)
	bySeq := make(map[uint64]media.Packet, len(in))
	var maxSeq uint64
	for _, p := range in {
		bySeq[p.Sequence] = p
		if p.Sequence > maxSeq {
			maxSeq = p.Sequence
		}
	}
	rb := transport.NewReorderBuffer(1)
	var ordered []media.Packet
	for _, p := range arrivals {
		ordered = append(ordered, rb.Accept(p)...)
	}
	recovered, err := rb.Recover(maxSeq, 0, func(seq uint64) (media.Packet, bool, error) {
		p, ok := bySeq[seq]
		return p, ok, nil
	})
	if err != nil {
		return nil, sim, rb, err
	}
	ordered = append(ordered, recovered...)
	return ordered, sim, rb, nil
}

func analyzeContent(opt Options, plans []cut.Plan) (audio.Report, []fingerprint.Signature, int, error) {
	samples := DemoAudio(opt.Seconds)
	report, err := audio.Analyze(samples, audio.DefaultOptions())
	if err != nil {
		return report, nil, 0, err
	}
	var sigs []fingerprint.Signature
	for i := range plans {
		block := audioBlock(samples, i, demoSegmentSeconds)
		review := DigestFloats(block)
		sigs = append(sigs, fingerprint.Build(plans[i].Output, block, DemoFrameSamples(i, opt.Seed), review, demoSegmentSeconds, fingerprint.DefaultOptions()))
	}
	_, confirmed, err := fingerprint.DetectDuplicates(sigs, fingerprint.DefaultOptions(), nil)
	if err != nil {
		return report, sigs, 0, err
	}
	return report, sigs, confirmed, nil
}

func firstVideoStream(info *source.Info) int {
	if info == nil {
		return 0
	}
	for _, s := range info.Streams {
		if s.Kind == "video" {
			return s.Index
		}
	}
	return 0
}
