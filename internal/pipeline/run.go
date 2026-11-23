// Package pipeline wires the readers, recovery, mapping, normalisation,
// checkpointing, remux and manifest stages into one demo run. Every stage keeps
// its error classification so the CLI can report a stable code, track and
// offset, and so an interrupted run can resume from intact blocks.
package pipeline

import (
	"os"

	"mediasync/internal/bandwidth"
	"mediasync/internal/checkpoint"
	trackmap "mediasync/internal/map"
	"mediasync/internal/metadata"
	"mediasync/internal/model"
)

// Run executes the pipeline. A data failure returns a partial report and a
// coded error; a deliberate interruption returns *InterruptError.
func Run(opt Options) (*Report, error) {
	rep := &Report{TargetKbps: opt.TargetKbps}
	if opt.InputDir == "" || opt.PackageDir == "" {
		return rep, model.NewError(model.ErrParamInvalid, 0, 0, "input and package directories are required")
	}
	if opt.CheckpointSamples <= 0 {
		opt.CheckpointSamples = 1
	}

	// Stage 1: parse the source and prove each track's coded bytes are read.
	// On a structural failure, report how much of a previous run is still
	// recoverable so an operator knows resuming is possible.
	tracks, stats, sourcePath, err := readSource(opt)
	if err != nil {
		attemptCheckpointRecovery(opt, rep)
		return rep, err
	}
	rep.TrackCount = len(tracks)
	for _, tr := range tracks {
		rep.SampleCount += len(tr.Samples)
	}

	// Stage 2: map tracks onto output targets. Any mapping failure aborts the
	// batch before a single block is written.
	mapping, err := trackmap.Resolve(trackmap.TargetsFromTracks(tracks), tracks, trackmap.Options{})
	if err != nil {
		return rep, err
	}

	// Stage 3: normalise display metadata with explicit overrides.
	for _, sel := range mapping.Selections {
		meta, err := metadata.Normalize(sel.Track, metadata.Overrides{Rotation: opt.Rotation})
		if err != nil {
			return rep, err
		}
		sel.Track.Meta = meta
		rep.Warnings = append(rep.Warnings, meta.Warnings...)
	}

	// Stage 4: recover the RTP input and record gaps as explicit intervals.
	gaps, err := recoverRTP(opt, rep)
	if err != nil {
		return rep, err
	}
	rep.Incomplete = gaps

	// Stage 5: write checkpoint blocks, resuming from any intact prefix. The
	// store lives beside the inputs so an interrupted run can be continued
	// with the same flags.
	store, err := checkpoint.Open(checkpointDir(opt.InputDir))
	if err != nil {
		return rep, err
	}
	if opt.Resume {
		valid, recovered, err := store.Load()
		if err != nil {
			return rep, err
		}
		if recovered {
			rep.CheckpointRecovered = true
		}
		if valid > 0 {
			rep.RecoveredBlocks = valid
		}
	}

	plan, _ := planBlocks(mapping, opt.CheckpointSamples)
	est, err := bandwidth.New(float64(opt.BandwidthKbps), 0.3)
	if err != nil {
		return rep, model.NewError(model.ErrParamInvalid, 0, 0, "%v", err)
	}
	// Resume skips exactly the blocks already committed, which the store
	// reports as a block count; sample counts are not comparable across tracks.
	_, _, alreadyCommitted := store.Stats()
	src, err := os.Open(sourcePath)
	if err != nil {
		return rep, err
	}
	defer src.Close()

	written := 0
	for i, blk := range plan {
		if i < alreadyCommitted {
			// Re-observe committed blocks so the reported rate covers the
			// whole program, not just the resumed tail.
			if buf, err := readBlock(src, blk); err == nil {
				est.ObserveBlock(len(buf), blockDuration(blk))
			}
			continue
		}
		buf, err := readBlock(src, blk)
		if err != nil {
			return rep, err
		}
		est.ObserveBlock(len(buf), blockDuration(blk))
		if _, err := store.AppendSamples(blk.trackID, buf, blk.samples); err != nil {
			return rep, err
		}
		written++
		if opt.InterruptAfter > 0 && written >= opt.InterruptAfter {
			_, _, blockCount := store.Stats()
			rep.CheckpointBlocks = blockCount
			rep.ActualKbps = est.AverageKbps()
			rep.FinalHash = checkpoint.HashString(store.Chain())
			return rep, &InterruptError{After: written}
		}
	}
	_, _, blockCount := store.Stats()
	rep.CheckpointBlocks = blockCount
	// The declared program rate is total coded bytes over the program's
	// timeline duration, not the sum of each track's own duration: tracks run
	// concurrently, so summing would double-count time.
	progBytes, progSecs := programThroughput(mapping)
	rep.ActualKbps = progBytes * 8 / 1000 / progSecs
	if rep.TargetKbps > 0 {
		rep.BitrateErrPct = (rep.ActualKbps - float64(rep.TargetKbps)) / float64(rep.TargetKbps) * 100
		if rep.BitrateErrPct < 0 {
			rep.BitrateErrPct = -rep.BitrateErrPct
		}
	}
	rep.FinalHash = checkpoint.HashString(store.Chain())

	// Stage 6: remux, verify and write manifests.
	if err := writePackage(opt, mapping, stats, rep, src); err != nil {
		return rep, err
	}
	return rep, nil
}
