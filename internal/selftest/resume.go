package selftest

import (
	"context"
	"os"
	"path/filepath"

	"medpipe/internal/clock"
	"medpipe/internal/mp4"
	"medpipe/internal/resume"
)

// interruptedRun performs a partial first pass that stops after committing a
// prefix of segments, mimicking a process interruption. It returns the
// checkpoint written so far and the index where work must resume.
func interruptedRun(ctx context.Context, inputPath, workDir string) (*resume.Checkpoint, int, error) {
	if !ensureCleanWorkDir(workDir) {
		return nil, 0, clock.E(clock.KindOther, "selftest.resume",
			clock.WithDetail("cannot prepare work directory %s", workDir))
	}
	sum, err := resume.HashFile(inputPath)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(inputPath)
	if err != nil {
		return nil, 0, clock.E(clock.KindOther, "selftest.resume", clock.WithWrap(err))
	}
	container, err := mp4.Parse(f, mp4.DefaultLimits)
	f.Close()
	if err != nil {
		return nil, 0, err
	}
	store, err := resume.NewStore(workDir)
	if err != nil {
		return nil, 0, err
	}
	cp := &resume.Checkpoint{
		InputPath: inputPath, InputSha256: sum, Tracks: trackStates(container),
	}

	// Commit only the first half of the planned segments, then "interrupt".
	total := int(container.Duration.Millis()) / segmentTargetMillis
	if total < 2 {
		total = 2
	}
	stopAfter := total / 2
	if stopAfter < 1 {
		stopAfter = 1
	}
	if _, err := Process(ctx, inputPath, workDir, cp, stopAfter); err != nil {
		return nil, 0, err
	}
	// The partial run's checkpoint on disk is the resume artifact.
	saved, err := store.Load()
	if err != nil {
		return nil, 0, err
	}
	return saved, stopAfter, nil
}

// ensureCleanWorkDir resets pipeline artifacts (the out directory and
// checkpoint) without touching generated source fixtures under <dir>/src.
func ensureCleanWorkDir(workDir string) bool {
	if err := os.RemoveAll(filepath.Join(workDir, "out")); err != nil {
		return false
	}
	if err := os.Remove(filepath.Join(workDir, "checkpoint.json")); err != nil &&
		!os.IsNotExist(err) {
		return false
	}
	if err := os.Remove(filepath.Join(workDir, "checkpoint.json.tmp")); err != nil &&
		!os.IsNotExist(err) {
		return false
	}
	return os.MkdirAll(filepath.Join(workDir, "out"), 0o755) == nil
}
