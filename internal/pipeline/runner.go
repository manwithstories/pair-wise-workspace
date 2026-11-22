package pipeline

import (
	"fmt"
	"os"
	"path/filepath"

	"mediaroute/internal/manifest"
	"mediaroute/internal/sim"
)

// Config controls one pipeline run.
type Config struct {
	WindowSize         int
	CheckpointInterval int    // items between checkpoints; 0 disables
	CrashAt            int    // item index at which to simulate a crash; <0 disables
	WorkDir            string // checkpoint directory
}

// Run processes the stream. With CrashAt set, it simulates a process
// crash at that item: in-memory state is discarded, the newest valid
// checkpoint is restored, and input is reprocessed from there — the
// result must equal the uninterrupted run segment for segment.
func Run(s *sim.Stream, cfg Config) (*Result, error) {
	eng := NewEngine(cfg.WindowSize)
	var cp *Checkpointer
	if cfg.WorkDir != "" {
		if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
			return nil, fmt.Errorf("create work dir: %w", err)
		}
		cp = NewCheckpointer(cfg.WorkDir, cfg.CheckpointInterval)
	}
	i := 0
	for i < len(s.Items) {
		if i == cfg.CrashAt && cp != nil {
			st, usedPrev, err := cp.Recover()
			if err != nil {
				return nil, err
			}
			eng = NewEngine(cfg.WindowSize)
			eng.Restore(st)
			eng.RollbackItems = i - st.ItemsProcessed
			if usedPrev {
				eng.Errs.Add(fmt.Errorf("checkpoint: current generation corrupt, fell back to previous"))
			}
			i = st.ItemsProcessed
			continue
		}
		eng.Step(s.Items[i])
		i++
		if cp != nil {
			if err := cp.MaybeSave(eng, i); err != nil {
				return nil, err
			}
		}
	}
	return eng.Finish(s), nil
}

// WriteOutput writes segment payloads and the manifest into dir.
func WriteOutput(dir string, res *Result) error {
	for i, seg := range res.Manifest.Segments {
		if err := manifest.WriteSegment(dir, seg, res.SegEvents[i]); err != nil {
			return fmt.Errorf("write segment %d: %w", i, err)
		}
	}
	if err := res.Manifest.Save(dir); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}
	return nil
}

// VerifyOutput recomputes every segment hash in dir and returns the
// number of mismatches against the manifest.
func VerifyOutput(dir string) (*manifest.Manifest, []string, int) {
	m, err := manifest.Load(dir)
	lines := []string{}
	if err != nil {
		return nil, append(lines, "error: "+err.Error()), 1
	}
	mismatches := 0
	for _, seg := range m.Segments {
		h, err := manifest.HashFile(dir, seg.Index)
		status := "ok"
		if err != nil {
			status = "missing"
			mismatches++
		} else if h != seg.SHA256 {
			status = "MISMATCH"
			mismatches++
		}
		lines = append(lines, fmt.Sprintf("segment %06d [%d, %d) sha256=%s %s",
			seg.Index, seg.StartNs, seg.EndNs, seg.SHA256, status))
	}
	return m, lines, mismatches
}

// CleanDir removes generated files (segments, manifest, checkpoints)
// inside dir without touching anything else.
func CleanDir(dir string) error {
	matches, err := filepath.Glob(filepath.Join(dir, "segment-*.seg"))
	if err != nil {
		return err
	}
	for _, f := range matches {
		if err := os.Remove(f); err != nil {
			return err
		}
	}
	for _, f := range []string{manifest.FileName, "checkpoint.json", "checkpoint.prev.json"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
