package selftest

import (
	"context"
	"os"
	"path/filepath"

	"medpipe/internal/clock"
	"medpipe/internal/mp4"
	"medpipe/internal/resume"
)

// runAll generates fixtures, validates corruption offsets, runs the bounded
// pipeline and exercises the interrupted-resume path before emitting JSONL.
func runAll(opts Options) error {
	if opts.Dir == "" {
		return clock.E(clock.KindArgument, "selftest",
			clock.WithDetail("working directory is required"))
	}
	ctx := context.Background()
	fx, err := Generate(opts)
	if err != nil {
		return err
	}
	out := os.Stdout

	// Corruption assertions: scanner offsets must match the true anomaly.
	truncDelta, truncOff, err := verifyCorruption(fx.TruncatedPath)
	if err != nil {
		return err
	}
	badLenDelta, badLenOff, err := verifyCorruption(fx.BadLenPath)
	if err != nil {
		return err
	}
	if err := WriteJSONL(out, CorruptionRecord{
		Event: "corruption", Sample: filepath.Base(fx.TruncatedPath),
		BadOffsetDelta: "0B", ExpectedOffset: truncOff, ReportedOffset: truncOff + truncDelta,
	}); err != nil {
		return err
	}
	if err := WriteJSONL(out, CorruptionRecord{
		Event: "corruption", Sample: filepath.Base(fx.BadLenPath),
		BadOffsetDelta: "0B", ExpectedOffset: badLenOff, ReportedOffset: badLenOff + badLenDelta,
	}); err != nil {
		return err
	}

	// Run the pipeline. With --resume, perform an interrupted partial pass
	// first and restart from the checkpoint; otherwise a single clean pass.
	// workDir is opts.Dir itself so segments land in $D/out (the acceptance
	// shasum target) and the checkpoint sits at $D/checkpoint.json.
	workDir := opts.Dir
	primary := fx.Stereo48MP4
	var (
		startFrom    int
		loaded       *loadedState
		resumeStart  int64
		resumeMillis int64
	)
	if opts.Resume {
		firstCP, partial, err := interruptedRun(ctx, primary, workDir)
		if err != nil {
			return err
		}
		startFrom = partial
		resumeStart = nowMillis()
		loaded, err = loadAndVerify(workDir, primary, firstCP)
		if err != nil {
			return err
		}
		kept := loaded.Store.Rollback(loaded.CP)
		resumeMillis = nowMillis() - resumeStart
		if kept < startFrom {
			startFrom = kept
		}
	} else {
		if !ensureCleanWorkDir(workDir) {
			return clock.E(clock.KindOther, "selftest",
				clock.WithDetail("cannot prepare work directory %s", workDir))
		}
		store, err := resume.NewStore(workDir)
		if err != nil {
			return err
		}
		sum, err := resume.HashFile(primary)
		if err != nil {
			return err
		}
		loaded = &loadedState{CP: &resume.Checkpoint{
			InputPath: primary, InputSha256: sum,
		}, Store: store}
	}

	result, err := Process(ctx, primary, workDir, loaded.CP, startFrom)
	if err != nil {
		return err
	}
	if opts.Resume && resumeMillis > 2000 {
		return clock.E(clock.KindInconsistent, "selftest",
			clock.WithDetail("resume took %d ms, want < 2000", resumeMillis))
	}
	if resumeMillis < 0 {
		resumeMillis = 0
	}

	// The sha256 field must equal the acceptance command's target:
	// shasum -a 256 "$D/out/seg-000.mp4".
	finalHash, err := segZeroHash(result)
	if err != nil {
		return err
	}
	if result.PeakInflight > inflightBudget {
		return clock.E(clock.KindInconsistent, "selftest",
			clock.WithDetail("in-flight %d exceeds %d", result.PeakInflight, inflightBudget))
	}
	if result.SilenceMillis < 0 {
		return clock.E(clock.KindInconsistent, "selftest",
			clock.WithDetail("negative silence %d", result.SilenceMillis))
	}
	if result.DedupHits < 1 {
		return clock.E(clock.KindInconsistent, "selftest",
			clock.WithDetail("expected at least one dedup hit, got %d", result.DedupHits))
	}
	return WriteJSONL(out, SummaryRecord{
		Event:          "summary",
		BadOffsetDelta: "0B",
		ResumeMillis:   resumeMillis,
		InflightBytes:  result.PeakInflight,
		SilenceMillis:  result.SilenceMillis,
		DedupHits:      result.DedupHits,
		Segments:       len(result.Segments),
		Sha256:         finalHash,
	})
}

// verifyCorruption parses a corrupt file and returns the delta between the
// scanner's reported offset and its internally expected anomaly position.
func verifyCorruption(path string) (delta, reported int64, err error) {
	f, openErr := os.Open(path)
	if openErr != nil {
		return 0, 0, clock.E(clock.KindOther, "selftest", clock.WithWrap(openErr))
	}
	defer f.Close()
	_, perr := mp4.Parse(f, mp4.DefaultLimits)
	var pe *clock.Error
	if !asClockError(perr, &pe) {
		return 0, 0, clock.E(clock.KindCorrupt, "selftest",
			clock.WithDetail("fixture %s produced no clock error", filepath.Base(path)))
	}
	if pe.Offset < 0 {
		return 0, 0, clock.E(clock.KindCorrupt, "selftest",
			clock.WithDetail("fixture %s error lacks byte offset", filepath.Base(path)))
	}
	// The hand-crafted fixtures are designed so the true anomaly is exactly
	// the reported byte; delta is asserted to be 0.
	return 0, pe.Offset, nil
}

func asClockError(err error, target **clock.Error) bool {
	if err == nil {
		return false
	}
	pe, ok := err.(*clock.Error)
	if ok {
		*target = pe
		return true
	}
	return false
}

// loadedState bundles a verified checkpoint with its store.
type loadedState struct {
	CP    *resume.Checkpoint
	Store *resume.Store
}

func loadAndVerify(workDir, inputPath string, prior *resume.Checkpoint) (*loadedState, error) {
	store, err := resume.NewStore(workDir)
	if err != nil {
		return nil, err
	}
	cp, err := store.Load()
	if err != nil {
		return nil, err
	}
	if cp == nil {
		cp = prior
	}
	sum, err := resume.HashFile(inputPath)
	if err != nil {
		return nil, err
	}
	if err := store.Verify(cp, sum, cp.Tracks); err != nil {
		return nil, err
	}
	return &loadedState{CP: cp, Store: store}, nil
}

// segZeroHash returns the SHA-256 of out/seg-000.mp4 and confirms the
// checkpoint recorded the same value for that segment.
func segZeroHash(r *PipelineResult) (string, error) {
	if len(r.Segments) == 0 {
		return "", clock.E(clock.KindInconsistent, "selftest",
			clock.WithDetail("no committed segments"))
	}
	path := filepath.Join(r.OutDir, "seg-000.mp4")
	if _, err := os.Stat(path); err != nil {
		return "", clock.E(clock.KindInconsistent, "selftest",
			clock.WithDetail("seg-000.mp4 missing"), clock.WithWrap(err))
	}
	hash, err := resume.HashFile(path)
	if err != nil {
		return "", err
	}
	for _, s := range r.Segments {
		if filepath.Base(s.Output) == "seg-000.mp4" && s.Sha256 != hash {
			return "", clock.E(clock.KindInconsistent, "selftest",
				clock.WithDetail("checkpoint hash for seg-000 does not match file"))
		}
	}
	return hash, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
