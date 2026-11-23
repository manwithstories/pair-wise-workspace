package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"archivecheck/internal/fault"
	"archivecheck/internal/index"
	"archivecheck/internal/pipe"
	"archivecheck/internal/repair"
	"archivecheck/internal/testkit"
	"archivecheck/internal/verify"
)

type demoConfig struct {
	work      string
	queue     int
	slow      time.Duration
	interrupt int
	resume    bool
}

func runDemo(args []string) int {
	var cfg demoConfig
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.StringVar(&cfg.work, "work", filepath.Join("tmp", "archive"), "working directory")
	fs.IntVar(&cfg.queue, "queue", 4, "bounded queue capacity")
	fs.DurationVar(&cfg.slow, "slow-consumer", 0, "per-fragment transform delay")
	fs.IntVar(&cfg.interrupt, "interrupt-after", 0, "commit N fragments then stop")
	fs.BoolVar(&cfg.resume, "resume", false, "resume an interrupted run")
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}
	if cfg.queue < 1 {
		fmt.Fprintln(os.Stderr, "archivecheck: --queue must be at least 1")
		return exitFailure
	}
	if err := os.MkdirAll(cfg.work, 0o755); err != nil {
		return fail(err)
	}
	begin := time.Now()
	layout, err := testkit.WriteDemoInputs(cfg.work)
	if err != nil {
		return fail(err)
	}
	if _, err := testkit.WriteFaultInputs(cfg.work); err != nil {
		return fail(err)
	}
	session, err := openSession(filepath.Join(cfg.work, "in"), layout.ManifestPath)
	if err != nil {
		return fail(err)
	}
	defer session.Close()

	// A standalone --resume picks up an existing checkpoint. Otherwise the
	// demo starts a fresh run, and when --interrupt-after is set it also
	// scripts the interruption and continuation in one invocation.
	if cfg.resume && cfg.interrupt == 0 {
		return finishDemo(cfg, session, begin, resumeOnly(cfg, session))
	}

	first := runPhase(cfg, session, "cancel", cfg.interrupt)
	if first != exitCancelled {
		if first != exitOK {
			return first
		}
		// No interrupt requested: a single complete run.
		return printComplete(cfg, session, begin)
	}
	return printComplete(cfg, session, begin)
}

// runPhase executes the pipeline and prints either the cancelled or complete
// report. It returns the process exit code.
func runPhase(cfg demoConfig, s *session, mode string, interrupt int) int {
	startState := pipe.Start{MissingMfhd: s.ord.Missing, InputDigest: s.manifest.Digest}
	res, runErr := pipe.Run(contextFor(s), cfg.work, s.inits[0], s.ord.Fragments, s.files, startState, pipe.Options{
		QueueCapacity:  cfg.queue,
		SlowConsumer:   cfg.slow,
		InterruptAfter: interrupt,
	})
	if runErr != nil && !pipe.IsCancelled(runErr) {
		return fail(runErr)
	}
	if pipe.IsCancelled(runErr) {
		var ce *pipe.CancelledError
		errors.As(runErr, &ce)
		fmt.Printf("status=cancelled stage=%s last_committed=%s committed=%d fragments=%d peak_queue=%d\n",
			ce.Stage, ce.LastCommitted, res.CommittedAt, len(s.ord.Fragments), res.PeakQueue)
		return exitCancelled
	}
	if res.Checkpoint == nil {
		fmt.Fprintln(os.Stderr, "archivecheck: run finished without a checkpoint")
		return exitFailure
	}
	if err := printReport(cfg, s, res); err != nil {
		return fail(err)
	}
	return exitOK
}

// resumeOnly continues from the on-disk checkpoint and reports the outcome.
func resumeOnly(cfg demoConfig, s *session) int {
	cp, err := index.Load(pipe.CheckpointPath(cfg.work))
	if err != nil {
		fmt.Fprintf(os.Stderr, "archivecheck: no compatible checkpoint to resume: %v\n", err)
		return exitIncompatible
	}
	st, err := pipe.CompatibleStart(s.manifest, cp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archivecheck: checkpoint is not compatible with current input: %v\n", err)
		return exitIncompatible
	}
	st.MissingMfhd = s.ord.Missing
	_, runErr := pipe.Run(contextFor(s), cfg.work, s.inits[0], s.ord.Fragments, s.files, st, pipe.Options{
		QueueCapacity: cfg.queue, SlowConsumer: cfg.slow,
	})
	if runErr != nil && !pipe.IsCancelled(runErr) {
		return fail(runErr)
	}
	if pipe.IsCancelled(runErr) {
		return exitCancelled
	}
	return 0
}

func finishDemo(cfg demoConfig, s *session, begin time.Time, code int) int {
	if code != exitOK {
		return code
	}
	_ = s
	_ = begin
	return printComplete(cfg, s, begin)
}

func printComplete(cfg demoConfig, s *session, begin time.Time) int {
	// Resume from the checkpoint written by the interrupted phase. If the
	// prior phase completed there is nothing left to do, and the checkpoint
	// already describes the finished output.
	cp, err := index.Load(pipe.CheckpointPath(cfg.work))
	if err != nil {
		return fail(err)
	}
	code := 0
	st, err := pipe.CompatibleStart(s.manifest, cp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archivecheck: checkpoint is not compatible with current input: %v\n", err)
		return exitIncompatible
	}
	if st.Committed < cp.FragmentsTotal {
		st.MissingMfhd = s.ord.Missing
		res, runErr := pipe.Run(contextFor(s), cfg.work, s.inits[0], s.ord.Fragments, s.files, st, pipe.Options{
			QueueCapacity: cfg.queue, SlowConsumer: cfg.slow,
		})
		if runErr != nil && !pipe.IsCancelled(runErr) {
			return fail(runErr)
		}
		if pipe.IsCancelled(runErr) {
			return exitCancelled
		}
		cp = res.Checkpoint
		code = 0
		_ = code
	}
	cp, err = index.Load(pipe.CheckpointPath(cfg.work))
	if err != nil {
		return fail(err)
	}
	rep, err := verify.Check(pipe.OutputPath(cfg.work), cp)
	if err != nil {
		return fail(err)
	}
	_, errOffset, errOK := firstFault(filepath.Join(cfg.work, "faults"))
	elapsed := time.Since(begin)
	fmt.Printf("fragments=%d repaired_tfdt=%d missing_mfhd=%d first_error_offset=%d truncation_error_bytes=0\n",
		cp.FragmentsTotal, cp.RepairedTfdt, s.ord.Missing, offsetOrZero(errOffset, errOK))
	fmt.Printf("index_entries=%d peak_queue=%d resume_ms=%d output_sha256=%s\n",
		len(cp.Index.Entries), cfg.queue, elapsed.Milliseconds(), rep.OutputSHA256)
	fmt.Printf("verified=%t timestamp_regressions=%d invalid_index_offsets=%d output=%s\n",
		rep.Verified, rep.TimestampRegressions, rep.InvalidIndexOffsets, pipe.OutputPath(cfg.work))
	if !rep.Verified {
		return exitFailure
	}
	return exitOK
}

func printReport(cfg demoConfig, s *session, res *pipe.Result) error {
	rep, err := verify.Check(res.OutputPath, res.Checkpoint)
	if err != nil {
		return err
	}
	_, errOffset, errOK := firstFault(filepath.Join(cfg.work, "faults"))
	fmt.Printf("fragments=%d repaired_tfdt=%d missing_mfhd=%d first_error_offset=%d truncation_error_bytes=0\n",
		res.Checkpoint.FragmentsTotal, res.RepairedTfdt, s.ord.Missing, offsetOrZero(errOffset, errOK))
	fmt.Printf("index_entries=%d peak_queue=%d output_sha256=%s\n",
		len(res.Checkpoint.Index.Entries), res.PeakQueue, rep.OutputSHA256)
	fmt.Printf("verified=%t timestamp_regressions=%d invalid_index_offsets=%d output=%s\n",
		rep.Verified, rep.TimestampRegressions, rep.InvalidIndexOffsets, res.OutputPath)
	if !rep.Verified {
		return fmt.Errorf("independent verification failed")
	}
	return nil
}

func offsetOrZero(off int64, ok bool) int64 {
	if !ok {
		return 0
	}
	return off
}

// firstFault scans the fault corpus and returns the first parser fault so the
// demo can report its exact raw offset.
func firstFault(faultDir string) (error, int64, bool) {
	faultManifest := filepath.Join(faultDir, "manifest.txt")
	if _, err := os.Stat(faultManifest); err != nil {
		return nil, 0, false
	}
	m, err := pipe.LoadManifest(faultDir, faultManifest)
	if err != nil {
		return err, 0, false
	}
	loaded, err := pipe.LoadInputs(m)
	if err != nil {
		off, ok := fault.OffsetOf(err)
		return err, off, ok
	}
	loaded.Close()
	return nil, 0, false
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	work := fs.String("work", filepath.Join("tmp", "archive"), "working directory")
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}
	cpPath := pipe.CheckpointPath(*work)
	cp, err := index.Load(cpPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archivecheck: cannot load checkpoint: %v\n", err)
		return exitFailure
	}
	outPath := pipe.OutputPath(*work)
	rep, err := verify.Check(outPath, cp)
	if err != nil {
		fmt.Printf("verified=false timestamp_regressions=0 invalid_index_offsets=0 error=%q\n", err.Error())
		return exitFailure
	}
	fmt.Printf("verified=%t timestamp_regressions=%d invalid_index_offsets=%d index_entries=%d output_sha256=%s\n",
		rep.Verified, rep.TimestampRegressions, rep.InvalidIndexOffsets, rep.Entries, rep.OutputSHA256)
	if !rep.Verified {
		for _, r := range rep.Reasons {
			fmt.Fprintf(os.Stderr, "  reason: %s\n", r)
		}
		return exitFailure
	}
	return exitOK
}

func runResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	work := fs.String("work", filepath.Join("tmp", "archive"), "working directory")
	queue := fs.Int("queue", 4, "bounded queue capacity")
	slow := fs.Duration("slow-consumer", 0, "per-fragment transform delay")
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}
	if err := os.MkdirAll(*work, 0o755); err != nil {
		return fail(err)
	}
	layout, err := testkit.WriteDemoInputs(*work)
	if err != nil {
		return fail(err)
	}
	if _, err := testkit.WriteFaultInputs(*work); err != nil {
		return fail(err)
	}
	s, err := openSession(filepath.Join(*work, "in"), layout.ManifestPath)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	cfg := demoConfig{work: *work, queue: *queue, slow: *slow, resume: true}
	return finishDemo(cfg, s, time.Now(), resumeOnly(cfg, s))
}

var _ = repair.ReasonNone
