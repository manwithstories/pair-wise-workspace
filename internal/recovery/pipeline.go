package recovery

import (
	"encoding/json"
	"path/filepath"

	"mediad/internal/clock"
	"mediad/internal/codec"
	"mediad/internal/fault"
	"mediad/internal/fixture"
	"mediad/internal/manifest"
)

const AdapterVersion = "adapter-v1"

// DefaultRTP returns the wrapper test vector generated with the fixtures.
func DefaultRTP(damage string) RTPConfig {
	w := fixture.DefaultWrapSpec(damage)
	return RTPConfig{
		ClockRate: int64(w.ClockRate), BaseSequence: w.BaseSequence,
		BaseTimestamp: w.BaseTimestamp, FrameTicks: int64(w.FrameTicks),
	}
}

type exec struct {
	opts   RunOptions
	params Params
	cp     *Checkpoint
	sum    Summary

	dash   *manifest.Manifest
	hls    *manifest.Manifest
	segs   []*loadedSegment
	tl     *clock.Timeline
	maps   []codec.MappedPacket
	pcm    []byte
	master []byte
}

// Run executes or resumes the pipeline according to content hashes.
func Run(opts RunOptions) (*Summary, error) {
	if opts.Root == "" {
		return nil, fault.New(fault.InvalidArgument, "", -1, "root is required")
	}
	p := opts.Params
	if p.Rate == 0 {
		p.Rate = 48000
	}
	if p.Layout == "" {
		p.Layout = "stereo"
	}
	if p.LUFS == 0 {
		p.LUFS = -18
	}
	if p.SourceRate == 0 {
		p.SourceRate = fixture.SourceRate
	}
	if p.SourceChannels == 0 {
		p.SourceChannels = fixture.SourceChannels
	}
	if opts.RTP.ClockRate == 0 {
		opts.RTP = DefaultRTP(p.Damage)
	}

	dash, hls, segs, hashes, err := loadSources(opts.Root)
	if err != nil {
		return nil, err
	}
	cp, resumed, err := loadCompatibleCheckpoint(opts.Root, hashes, p)
	if err != nil {
		return nil, err
	}

	e := &exec{
		opts: opts, params: p, cp: cp,
		sum:  Summary{TargetLUFS: p.LUFS, OutputSampleRate: p.Rate, Channels: 2},
		dash: dash, hls: hls, segs: segs, tl: clock.NewTimeline("av"),
	}
	e.sum.CheckpointKey = cp.Key
	e.sum.ConfigHash = cp.ConfigHash

	stages := []func() error{
		e.stageManifest, e.stageFMP4, e.stageCodec, e.stageResample,
		e.stageLoudness, e.stageKeyframe, e.stageRTP, e.stageLedger, e.stageOutput,
	}
	for _, stage := range stages {
		if err := stage(); err != nil {
			return nil, err
		}
	}
	e.sum.OK = true
	e.sum.ResumeMatch = opts.Resume && resumed
	if err := e.writeSummary(); err != nil {
		return nil, err
	}
	return &e.sum, nil
}

func loadCompatibleCheckpoint(root string, hashes []string, p Params) (*Checkpoint, bool, error) {
	_, _, key, err := BuildKey(hashes, p, AdapterVersion)
	if err != nil {
		return nil, false, err
	}
	path := filepath.Join(root, "work", "checkpoint.json")
	old, err := LoadCheckpoint(path)
	if err != nil {
		if f, ok := fault.As(err); ok && f.Code == fault.IO {
			cp, nerr := NewCheckpoint(hashes, p, AdapterVersion)
			return cp, false, nerr
		}
		return nil, false, err
	}
	if old.Key != key {
		// Inputs, parameters or adapter version changed: old downstream state is invalid.
		cp, nerr := NewCheckpoint(hashes, p, AdapterVersion)
		if nerr != nil {
			return nil, false, nerr
		}
		return cp, false, nil
	}
	return old, true, nil
}

func (e *exec) finishStage(stage string) error {
	e.cp.MarkStage(stage)
	if err := e.cp.SaveAtomic(e.opts.Root); err != nil {
		return err
	}
	if e.params.Interrupt == stage {
		e.cp.InterruptedAt = stage
		if err := e.cp.SaveAtomic(e.opts.Root); err != nil {
			return err
		}
		return fault.New(fault.Interrupted, filepath.Join(e.opts.Root, "work", "checkpoint.json"), -1,
			"interrupted after %s; run resume", stage)
	}
	return nil
}

func (e *exec) artifactPath(name string) string {
	return filepath.Join(e.opts.Root, "work", name)
}

func (e *exec) outputPath(name string) string {
	return filepath.Join(e.opts.Root, "output", name)
}

func (e *exec) writeSummary() error {
	data, err := json.MarshalIndent(e.sum, "", "  ")
	if err != nil {
		return fault.New(fault.CheckpointCorrupt, e.outputPath("summary.json"), -1, "encode summary: %v", err)
	}
	return writeFileAtomic(e.outputPath("summary.json"), data)
}
