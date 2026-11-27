package pipeline

import (
	"fmt"
	"os"

	"avrepair/internal/journal"
)

// Run executes the repair and returns a report.
func Run(opt Options) (*Report, error) {
	if opt.Input == "" || opt.StateDir == "" || opt.OutputDir == "" {
		return nil, fmt.Errorf("pipeline: input, state and output are required")
	}
	if err := validateInterrupt(opt.InterruptAt); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opt.OutputDir, 0o755); err != nil {
		return nil, err
	}
	store, err := journal.Open(opt.StateDir)
	if err != nil {
		return nil, err
	}
	digest, err := journal.DigestInput(opt.Input)
	if err != nil {
		return nil, err
	}
	rep := &Report{Input: opt.Input, StateDir: opt.StateDir, OutputDir: opt.OutputDir, InputDigest: digest}
	c := &runContext{opt: opt, store: store, digest: digest, report: rep, captures: map[string]*capture{}}

	reuse := map[string]bool{}
	if opt.Resume {
		cp, err := store.LoadCheckpoint()
		if err != nil {
			return nil, err
		}
		if cp != nil && cp.InputDigest == digest {
			for _, stage := range store.Reused(digest) {
				reuse[stage] = true
			}
			rep.Resumed = true
			// Carry the persisted result hash forward so resuming a
			// finalized run never re-derives a different value.
			if cp.ResultSHA != "" {
				c.hash = cp.ResultSHA
			}
		}
	}

	// The parse stage is always re-run: it is cheap, and it rebuilds the
	// in-memory captures needed by every later stage. Its completion is
	// still recorded so reused_stages reflects prior work.
	if err := c.stageParse(); err != nil {
		return nil, err
	}
	if reuse[StageParse] {
		rep.ReusedNames = append(rep.ReusedNames, StageParse)
	}
	if opt.InterruptAt == StageParse {
		return c.interrupt(StageParse)
	}

	if err := c.stageRecover(reuse[StageRecover]); err != nil {
		return nil, err
	}
	if opt.InterruptAt == StageRecover {
		return c.interrupt(StageRecover)
	}

	if reuse[StageRepair] {
		if err := c.loadRepairArtifact(); err != nil {
			return nil, err
		}
		rep.ReusedNames = append(rep.ReusedNames, StageRepair)
	} else {
		if err := c.stageRepair(); err != nil {
			return nil, err
		}
		if opt.InterruptAt == StageRepair {
			return c.interrupt(StageRepair)
		}
	}

	if reuse[StageIndex] && c.indexOnDiskValid() {
		if err := c.loadIndexArtifact(); err != nil {
			return nil, err
		}
		rep.ReusedNames = append(rep.ReusedNames, StageIndex)
	} else {
		if err := c.stageIndex(); err != nil {
			return nil, err
		}
		if opt.InterruptAt == StageIndex {
			return c.interrupt(StageIndex)
		}
	}

	if err := c.stageFinalize(); err != nil {
		return nil, err
	}
	for _, stage := range []string{StageParse, StageRecover, StageRepair, StageIndex} {
		if reuse[stage] && !contains(rep.ReusedNames, stage) {
			rep.ReusedNames = append(rep.ReusedNames, stage)
		}
	}
	rep.ReusedStages = len(rep.ReusedNames)
	return rep, nil
}

func validateInterrupt(stage string) error {
	switch stage {
	case "", StageParse, StageRecover, StageRepair, StageIndex:
		return nil
	default:
		return fmt.Errorf("pipeline: unknown interrupt stage %q", stage)
	}
}
