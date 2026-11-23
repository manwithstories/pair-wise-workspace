package pipeline

import (
	"path/filepath"

	"mediasync/internal/checkpoint"
)

func checkpointDir(inputDir string) string { return filepath.Join(inputDir, "checkpoints") }

// attemptCheckpointRecovery inspects an existing checkpoint store without
// writing. It records how many intact blocks remain so a failed run still tells
// the operator whether a resume can pick up where it left off.
func attemptCheckpointRecovery(opt Options, rep *Report) {
	store, err := checkpoint.Open(checkpointDir(opt.InputDir))
	if err != nil {
		return
	}
	valid, recovered, err := store.Load()
	if err != nil {
		return
	}
	rep.CheckpointRecovered = true
	rep.RecoveredBlocks = valid
	if !recovered && valid == 0 {
		rep.RecoveredBlocks = 0
	}
}

// programThroughput returns total coded bytes and the longest track duration in
// seconds. That is the timeline a viewer experiences; summing per-track
// durations would double-count concurrent tracks.
