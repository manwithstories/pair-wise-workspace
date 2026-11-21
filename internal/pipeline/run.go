// Package pipeline wires the probe, RTP, repair, index, manifest and report
// subsystems together and records every stage through the WAL.
package pipeline

import (
	"fmt"
	"os"
	"path/filepath"

	"mediad/internal/model"
	"mediad/internal/txn"
)

// FixtureNames are the files the demo writes into its working directory.
const (
	MP4Name = "sample.mp4"
	MXFName = "sample.mxf"
)

// The demo's media is fixed and deliberately small: this is a demonstration
// of the library, not a transcoder. Sizes are chosen so a healthy delivery
// lands 2% under its target, matching the delivery spec's safety margin.
// rtpTicksPerFrame is the demo RTP clock's ticks between packets.
const rtpTicksPerFrame = 90000 / 30

const (
	videoFrames      = 120
	videoDelta       = 1001
	videoTimescale   = 30000
	videoBytes       = 100000
	audioFrames      = 192
	audioDelta       = 1000
	audioTimescale   = 48000
	audioBytes       = 667
	mxfEssenceBytes  = 100000
	headroomFraction = 0.02
)

// Run executes the complete demo pipeline. For a given seed and fault list it
// is deterministic: repeated runs produce the same manifest digest.
func Run(opt DemoOptions) (Result, error) {
	var res Result
	if err := opt.validate(); err != nil {
		return res, err
	}
	mp4Path := filepath.Join(opt.Dir, MP4Name)
	mxfPath := filepath.Join(opt.Dir, MXFName)
	if err := writeFixtures(opt.Seed, mp4Path, mxfPath); err != nil {
		return res, err
	}
	txnDir := filepath.Join(opt.Dir, "txn")
	if !opt.Resume {
		if err := os.RemoveAll(txnDir); err != nil {
			return res, model.Wrap(model.KindInternal, "pipeline", "reset transaction directory", err)
		}
	}

	txm, err := txn.Open(txnDir, 0)
	if err != nil {
		return res, model.Wrap(model.KindRecovery, "pipeline", "open transaction log", err)
	}
	defer txm.Close()
	if dirty, reason := txm.CorruptTail(); dirty && !opt.Resume {
		res.Notes = append(res.Notes, "discarded a corrupt WAL tail: "+reason)
	}

	r := &runner{opt: opt, paths: map[string]string{"mp4": mp4Path, "mxf": mxfPath}, txm: txm, res: &res}
	limit := -1
	if opt.InterruptAfter != "" {
		limit = stageIndex(opt.InterruptAfter)
	}
	if opt.Resume && limit < 0 {
		// A plain resume over an existing log from a previous process:
		// recover first, then report the boundary the dead process reached
		// and the stage this process continues from.
		if dirty, reason := txm.CorruptTail(); dirty {
			res.Notes = append(res.Notes, "recovery discarded a torn WAL tail: "+reason)
		}
		stage, err := txm.Recover()
		if err != nil {
			return res, err
		}
		res.RollbackStage = txm.RollbackStage()
		if res.RollbackStage == "" {
			// A torn tail with no checksum failure means the last stage
			// committed cleanly and the next one died mid-commit.
			if last := lastCommitted(txm.Committed()); last != "" {
				res.RollbackStage = string(last)
			}
		}
		if stage != "" {
			res.ResumeStage = string(stage)
		}
		committed := txm.Committed()
		for _, s := range txn.Order {
			if _, ok := committed[s]; ok {
				res.ReusedStages = append(res.ReusedStages, string(s))
			}
		}
	}

	// Phase 1: run through the stage after which the process "dies".
	for i, s := range txn.Order {
		if limit >= 0 && i > limit {
			break
		}
		if err := r.execute(s); err != nil {
			return res, err
		}
	}
	if limit < 0 {
		return res, r.finish()
	}

	// Phase 2: die mid-commit. The WAL keeps a partial record and a torn
	// tail, and the next stage's artifact was never written.
	if err := r.simulateCrash(); err != nil {
		return res, err
	}
	if !opt.Resume {
		return res, model.New(model.KindRecovery, "pipeline",
			fmt.Sprintf("interrupted after %s: artifacts for the next stage are incomplete; rerun with --resume to continue", opt.InterruptAfter))
	}

	// Phase 3: reopen and recover, reusing only checksum-verified stages.
	if err := txm.Close(); err != nil {
		return res, model.Wrap(model.KindRecovery, "pipeline", "close transaction log", err)
	}
	txm2, err := txn.Open(txnDir, 0)
	if err != nil {
		return res, model.Wrap(model.KindRecovery, "pipeline", "reopen transaction log", err)
	}
	defer txm2.Close()
	if dirty, reason := txm2.CorruptTail(); dirty {
		res.Notes = append(res.Notes, "recovery discarded a torn WAL tail: "+reason)
	}
	resumeFrom, err := txm2.Recover()
	if err != nil {
		return res, err
	}
	res.RollbackStage = txm2.RollbackStage()
	if res.RollbackStage == "" {
		res.RollbackStage = string(opt.InterruptAfter)
	}
	if resumeFrom == "" {
		// Every committed stage verified, but the crashed stage never
		// committed, so its successor is the one to resume at.
		resumeFrom = txn.Order[limit+1]
	}
	res.ResumeStage = string(resumeFrom)
	committed := txm2.Committed()
	for _, s := range txn.Order {
		if _, ok := committed[s]; ok {
			res.ReusedStages = append(res.ReusedStages, string(s))
		}
	}
	r.txm = txm2

	// Phase 4: continue from the first uncommitted stage.
	for i, s := range txn.Order {
		if i < stageIndex(resumeFrom) {
			continue
		}
		if err := r.execute(s); err != nil {
			return res, err
		}
	}
	return res, r.finish()
}

// lastCommitted returns the latest pipeline stage with a valid checkpoint.
func lastCommitted(m map[txn.Stage]txn.Checkpoint) txn.Stage {
	var last txn.Stage
	for _, s := range txn.Order {
		if _, ok := m[s]; ok {
			last = s
		}
	}
	return last
}

func (o *DemoOptions) validate() error {
	if o.Dir == "" {
		return model.New(model.KindInvalidInput, "pipeline", "working directory is required")
	}
	if o.SeeksPerTrack <= 0 {
		o.SeeksPerTrack = 11
	}
	if o.MaxBurst <= 0 {
		o.MaxBurst = 8
	}
	if o.InterruptAfter != "" && !validStage(o.InterruptAfter) {
		return model.New(model.KindInvalidInput, "pipeline",
			fmt.Sprintf("unknown interrupt stage %q", o.InterruptAfter))
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return model.Wrap(model.KindInternal, "pipeline", "create working directory", err)
	}
	return nil
}

// runner carries state across stages of one process run.
