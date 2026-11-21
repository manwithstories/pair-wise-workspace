package pipeline

import (
	"fmt"
	"os"

	"mediad/internal/index"
	"mediad/internal/model"
	"mediad/internal/txn"
)

func buildIndexes(txm *txn.Manager, container model.Container, opt DemoOptions, res *Result) (int, error) {
	indexes := make([]index.TrackIndex, 0, len(container.Tracks))
	mismatches := 0
	for _, t := range container.Tracks {
		idx, err := index.Rebuild(t)
		if err != nil {
			return 0, err
		}
		bad, msgs, err := index.Verify(t, idx, opt.SeeksPerTrack)
		if err != nil {
			return 0, err
		}
		mismatches += bad
		for _, m := range msgs {
			res.Notes = append(res.Notes, fmt.Sprintf("track %d %s", t.ID, m))
		}
		indexes = append(indexes, idx)
	}
	if _, err := txm.Commit(txn.StageIndex, mustJSON(indexes)); err != nil {
		return 0, err
	}
	return mismatches, nil
}

// recoverCorruptIndex damages a committed artifact, runs recovery, and
// rebuilds the rolled-back stage. It proves a stage whose checksum fails is
// never reused.
func recoverCorruptIndex(txm *txn.Manager, container model.Container, opt DemoOptions, res *Result) (int, error) {
	path := txm.ArtifactPath(txn.StageIndex)
	if err := os.WriteFile(path, []byte(`{"corrupted":true}`), 0o644); err != nil {
		return 0, model.Wrap(model.KindRecovery, "pipeline", "corrupt index artifact", err)
	}
	res.InjectedFaults++
	// The log has grown since Open; refresh the manager's view so recovery
	// judges the artifact that is actually on disk now.
	if err := txm.Rescan(); err != nil {
		return 0, model.Wrap(model.KindRecovery, "pipeline", "rescan transaction log", err)
	}
	stage, err := txm.Recover()
	if err != nil {
		return 0, err
	}
	res.RollbackStage = txm.RollbackStage()
	if stage != "" {
		res.ResumeStage = string(stage)
	}
	res.Notes = append(res.Notes,
		"committed index artifact failed its checksum; recovery rolled back to the last atomic boundary")
	return buildIndexes(txm, container, opt, res)
}
