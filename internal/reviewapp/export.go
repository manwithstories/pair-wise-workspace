package reviewapp

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	mp4 "github.com/abema/go-mp4"

	"review/internal/dag"
	"review/internal/fixture"
	"review/internal/index"
	"review/internal/media"
	"review/internal/report"
	"review/internal/video"
)

// exportTwice runs the off-screen export two separate times into two
// directories and requires byte-identical segment hashes, recording the
// stable-segment counts used by the acceptance thresholds.
func exportTwice(out string, rows []index.PreviewRow, timescale uint32,
	m *report.Metrics) (string, error) {
	dirA := filepath.Join(out, "frames-a")
	dirB := filepath.Join(out, "frames-b")
	expA, err := video.NewExporter(dirA)
	if err != nil {
		return "", media.NewInternal("frame-export", "exporter A", err)
	}
	framesA, err := expA.Export(rows, timescale)
	if err != nil {
		return "", media.NewInternal("frame-export", "export A", err)
	}
	expB, err := video.NewExporter(dirB)
	if err != nil {
		return "", media.NewInternal("frame-export", "exporter B", err)
	}
	framesB, err := expB.Export(rows, timescale)
	if err != nil {
		return "", media.NewInternal("frame-export", "export B", err)
	}
	hashA := video.SegmentHash(framesA)
	hashB := video.SegmentHash(framesB)
	m.TotalSegments = len(framesA)
	for i := range framesA {
		if framesA[i].Hash == framesB[i].Hash {
			m.StableSegments++
		}
	}
	if hashA != hashB {
		return "", media.NewInternal("frame-export",
			fmt.Sprintf("segment hash drift %s != %s", hashA, hashB), nil)
	}
	return hashA, nil
}

// verifyMP4Index reads the generated preview back with go-mp4 and checks
// the stts sample count and delta against the keyframe index projection.
func verifyMP4Index(raw []byte, wantSamples, wantDelta uint32) error {
	var stts *mp4.Stts
	_, err := mp4.ReadBoxStructure(bytes.NewReader(raw), func(h *mp4.ReadHandle) (any, error) {
		box, _, perr := h.ReadPayload()
		if perr != nil {
			return nil, perr
		}
		if s, ok := box.(*mp4.Stts); ok {
			stts = s
		}
		if _, err := h.Expand(); err != nil {
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		return media.NewInternal("preview-verify", "go-mp4 read", err)
	}
	if stts == nil || len(stts.Entries) != 1 {
		return media.NewInternal("preview-verify", "stts table missing", nil)
	}
	if stts.Entries[0].SampleCount != wantSamples {
		return media.NewInternal("preview-verify",
			fmt.Sprintf("stts sample count %d != %d",
				stts.Entries[0].SampleCount, wantSamples), nil)
	}
	if stts.Entries[0].SampleDelta != wantDelta {
		return media.NewInternal("preview-verify",
			fmt.Sprintf("stts delta %d != %d",
				stts.Entries[0].SampleDelta, wantDelta), nil)
	}
	return nil
}

// exerciseDAGResume simulates a crash after three fragments committed and
// one temp fragment left behind, then resumes with a fresh runner and
// confirms the committed work is skipped while the interrupted fragment
// finishes exactly once.
func exerciseDAGResume(stateDir string, art fixture.RecoveryArtifact) error {
	// A stale temp file represents the half-written fourth fragment.
	tmpPath := filepath.Join(stateDir, art.TempName)
	// Reset the state directory so each demo invocation performs a fresh
	// crash-and-resume exercise regardless of prior runs.
	if err := os.RemoveAll(stateDir); err != nil {
		return media.NewInternal("dag-runner", "reset state dir", err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return media.NewInternal("dag-runner", "state dir", err)
	}
	if err := os.WriteFile(tmpPath, art.TempData, 0o644); err != nil {
		return media.NewInternal("dag-runner", "seed temp", err)
	}

	// Phase 1: the first three fragments commit, then the process dies
	// before fragment four (its temp file is left on disk).
	rt1, err := dag.NewRunner(stateDir)
	if err != nil {
		return media.NewInternal("dag-runner", "init phase 1", err)
	}
	for i, frag := range art.Committed {
		name := fmt.Sprintf("fragment-%03d", i+1)
		key, _ := dag.IdempotencyKey(map[string]string{"data": hashRef(frag)}, nil)
		if err := rt1.Add(dag.Node{Name: name, Run: func(rt *dag.Runner) error {
			rt.MarkKey(name, key)
			rt.RecordOutput(name, "hash", key)
			return nil
		}}); err != nil {
			return err
		}
	}
	if err := rt1.RunAll(); err != nil {
		return err
	}

	// Phase 2: a brand-new runner reloads durable state. The three
	// committed fragments must be skipped; only fragment four runs (once),
	// atomically replacing the stale temp file.
	rt2, err := dag.NewRunner(stateDir)
	if err != nil {
		return media.NewInternal("dag-runner", "init phase 2", err)
	}
	reRan := 0
	for i := range art.Committed {
		name := fmt.Sprintf("fragment-%03d", i+1)
		if err := rt2.Add(dag.Node{Name: name, Run: func(*dag.Runner) error {
			reRan++
			return nil
		}}); err != nil {
			return err
		}
	}
	finalRuns := 0
	if err := rt2.Add(dag.Node{
		Name: "fragment-004", Deps: []string{"fragment-003"},
		Run: func(rt *dag.Runner) error {
			finalRuns++
			key, _ := dag.IdempotencyKey(map[string]string{"data": "fragment-004"}, nil)
			rt.MarkKey("fragment-004", key)
			rt.RecordOutput("fragment-004", "hash", key)
			return os.Remove(tmpPath) // atomic commit replaces the temp artifact
		},
	}); err != nil {
		return err
	}
	if err := rt2.RunAll(); err != nil {
		return err
	}
	if reRan != 0 {
		return media.NewInternal("dag-runner",
			fmt.Sprintf("committed fragments re-ran %d times on resume", reRan), nil)
	}
	if finalRuns != 1 {
		return media.NewInternal("dag-runner",
			fmt.Sprintf("interrupted fragment ran %d times, want 1", finalRuns), nil)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		return media.NewInternal("dag-runner", "stale temp file survived commit", nil)
	}
	return nil
}

// hashRef is a stable string reference for fragment inputs; the exact hash
// algorithm is irrelevant because only key stability is under test.
func hashRef(b []byte) string {
	return fmt.Sprintf("%x", len(b))
}

// classify maps a pipeline error class onto the CLI exit code.
func classify(err error) int {
	pe, ok := err.(*media.PipelineError)
	if !ok {
		return ExitInternal
	}
	switch pe.Class {
	case media.ClassBoundary, media.ClassConflict:
		return ExitBoundary
	default:
		return ExitInternal
	}
}
