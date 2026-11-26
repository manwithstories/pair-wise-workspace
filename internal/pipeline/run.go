package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/model"
	"mediad/internal/trackmap"
)

var ErrInterrupted = errors.New("pipeline interrupted at committed sample boundary")

type Options struct {
	InputPath       string
	OutputPath      string
	CheckpointPath  string
	CachePath       string
	Requests        []trackmap.Request
	InterruptAfter  int
	CopyBufferBytes int
}

type Result struct {
	CacheState     string
	CheckpointPath string
	Checkpoint     *Checkpoint
	OutputSHA256   string
	Interrupted    bool
	Verification   *LosslessReport
}

type sampleRef struct{ track, sample int }

func Run(opts Options) (*Result, error) {
	if opts.InputPath == "" || opts.OutputPath == "" {
		return nil, model.New(model.CodeInvalidArgument, model.StageCLI, "input and output paths are required")
	}
	if opts.CheckpointPath == "" {
		opts.CheckpointPath = opts.OutputPath + ".checkpoint.json"
	}
	if opts.CopyBufferBytes <= 0 {
		opts.CopyBufferBytes = 64 << 10
	}
	cached, err := LoadOrBuildIndex(opts.InputPath, opts.CachePath)
	if err != nil {
		return nil, err
	}
	idx := cached.Index
	plan, err := trackmap.BuildPlan(idx, opts.Requests)
	if err != nil {
		return nil, err
	}
	totalBytes := planTotalBytes(plan)
	mdatHeaderBytes, dataStart, _, err := mdatHeader(totalBytes)
	if err != nil {
		return nil, err
	}
	order, offsets := layoutPlan(plan, dataStart)

	ckpt, err := LoadCheckpoint(opts.CheckpointPath)
	if err != nil {
		return nil, err
	}
	if ckpt != nil && !ckpt.BindingsMatch(idx.InputSHA256, plan.Digest) {
		return nil, model.New(model.CodeIntegrity, model.StageCheckpoint, "checkpoint is bound to different input or mapping").
			WithDigest(idx.InputSHA256, ckpt.InputSHA256)
	}
	if ckpt != nil && (ckpt.TotalSamples != len(order) || ckpt.TotalMediaBytes != totalBytes) {
		return nil, model.New(model.CodeCheckpointCorrupt, model.StageCheckpoint, "checkpoint sample boundary does not match plan")
	}

	in, err := os.Open(opts.InputPath)
	if err != nil {
		return nil, model.Wrap(model.CodeInvalidArgument, model.StagePipeline, "open input media", err).WithSource(opts.InputPath)
	}
	defer in.Close()

	res := &Result{CacheState: cached.State, CheckpointPath: opts.CheckpointPath}
	var out *os.File
	var prefixHash hash.Hash
	start := 0

	if ckpt == nil {
		if err := os.MkdirAll(filepath.Dir(opts.OutputPath), 0o755); err != nil {
			return nil, model.Wrap(model.CodeInternal, model.StagePipeline, "create output directory", err)
		}
		out, err = os.OpenFile(opts.OutputPath, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o644)
		if err != nil {
			return nil, model.Wrap(model.CodeInternal, model.StagePipeline, "create output", err).WithSource(opts.OutputPath)
		}
		defer out.Close()
		ftyp := outputFTYP()
		if _, err := out.Write(ftyp); err != nil {
			return nil, model.Wrap(model.CodeInternal, model.StagePipeline, "write ftyp", err)
		}
		if _, err := out.Write(mdatHeaderBytes); err != nil {
			return nil, model.Wrap(model.CodeInternal, model.StagePipeline, "write mdat header", err)
		}
		h := sha256.New()
		_, _ = h.Write(ftyp)
		_, _ = h.Write(mdatHeaderBytes)
		c := &Checkpoint{
			InputSHA256: idx.InputSHA256, MappingDigest: plan.Digest, OutputPath: opts.OutputPath,
			TotalSamples: len(order), TotalMediaBytes: totalBytes, PrefixBytes: dataStart,
			PrefixSHA256: hex.EncodeToString(h.Sum(nil)),
			Boundaries:   make([]SampleBoundary, len(plan.Tracks)),
		}
		for i, pt := range plan.Tracks {
			c.Boundaries[i] = SampleBoundary{OutputTrackID: pt.OutputID}
		}
		if err := SaveCheckpoint(opts.CheckpointPath, c); err != nil {
			return nil, err
		}
		ckpt, prefixHash, start = c, h, 0
	} else if ckpt.Complete {
		sha, err := HashFileSHA256(opts.OutputPath)
		if err != nil {
			return nil, err
		}
		if sha != ckpt.OutputSHA256 {
			return nil, model.New(model.CodeIntegrity, model.StagePipeline, "completed output SHA-256 does not match checkpoint").
				WithDigest(ckpt.OutputSHA256, sha)
		}
		report, err := VerifyLossless(opts.InputPath, opts.OutputPath, plan)
		if err != nil {
			return nil, err
		}
		res.Checkpoint, res.OutputSHA256, res.Verification = ckpt, sha, report
		return res, nil
	} else {
		out, err = os.OpenFile(opts.OutputPath, os.O_RDWR, 0o644)
		if err != nil {
			return nil, model.Wrap(model.CodeInvalidArgument, model.StagePipeline, "open resumable output", err).WithSource(opts.OutputPath)
		}
		defer out.Close()
		h, err := ckpt.VerifyAndTruncatePrefix(out)
		if err != nil {
			return nil, err
		}
		prefixHash = h
		start = ckpt.CommittedSamples
	}

	buf := make([]byte, opts.CopyBufferBytes)
	for committed := start; committed < len(order); committed++ {
		ref := order[committed]
		s := plan.Tracks[ref.track].Samples[ref.sample]
		sr := io.NewSectionReader(in, s.Offset, int64(s.Size))
		mw := io.MultiWriter(out, prefixHash)
		n, err := io.CopyBuffer(mw, sr, buf)
		if err != nil {
			return nil, model.Wrap(model.CodeInternal, model.StagePipeline, "copy sample", err).
				WithTrack(plan.Tracks[ref.track].OutputID)
		}
		if n != int64(s.Size) {
			return nil, model.New(model.CodeIntegrity, model.StagePipeline, "short sample copy").WithOffset(s.Offset)
		}
		ckpt.CommittedSamples = committed + 1
		ckpt.PrefixBytes += int64(s.Size)
		ckpt.PrefixSHA256 = hex.EncodeToString(prefixHash.Sum(nil))
		ckpt.Boundaries[ref.track] = SampleBoundary{
			OutputTrackID: plan.Tracks[ref.track].OutputID,
			Committed:     ref.sample + 1,
		}
		if err := SaveCheckpoint(opts.CheckpointPath, ckpt); err != nil {
			return nil, err
		}
		if opts.InterruptAfter > 0 && ckpt.CommittedSamples >= opts.InterruptAfter {
			res.Interrupted = true
			res.Checkpoint = ckpt
			return res, ErrInterrupted
		}
	}

	moov, err := buildOutputMoov(plan, offsets)
	if err != nil {
		return nil, err
	}
	if _, err := out.Write(moov); err != nil {
		return nil, model.Wrap(model.CodeInternal, model.StagePipeline, "write moov", err)
	}
	if err := out.Close(); err != nil {
		return nil, model.Wrap(model.CodeInternal, model.StagePipeline, "close output", err)
	}
	out = nil
	report, err := VerifyLossless(opts.InputPath, opts.OutputPath, plan)
	if err != nil {
		return nil, err
	}
	outSHA, err := HashFileSHA256(opts.OutputPath)
	if err != nil {
		return nil, err
	}
	ckpt.Complete = true
	ckpt.OutputSHA256 = outSHA
	if err := SaveCheckpoint(opts.CheckpointPath, ckpt); err != nil {
		return nil, err
	}
	res.Checkpoint, res.OutputSHA256, res.Verification = ckpt, outSHA, report
	return res, nil
}

func planTotalBytes(plan *trackmap.Plan) int64 {
	var total int64
	for _, pt := range plan.Tracks {
		for _, s := range pt.Samples {
			total += int64(s.Size)
		}
	}
	return total
}

func layoutPlan(plan *trackmap.Plan, dataStart int64) ([]sampleRef, [][]int64) {
	var order []sampleRef
	offsets := make([][]int64, len(plan.Tracks))
	cursor := dataStart
	for ti, pt := range plan.Tracks {
		offsets[ti] = make([]int64, len(pt.Samples))
		for si, s := range pt.Samples {
			order = append(order, sampleRef{ti, si})
			offsets[ti][si] = cursor
			cursor += int64(s.Size)
		}
	}
	return order, offsets
}
