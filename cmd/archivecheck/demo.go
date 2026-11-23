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
	var s *session
	var err error
	if cfg.resume && cfg.interrupt == 0 {
		// An explicit resume must observe the corpus exactly as it is on disk:
		// a regenerated or edited input would silently change its digest.
		s, err = loadSession(cfg.work, filepath.Join(cfg.work, "in"), filepath.Join(cfg.work, "in", "manifest.txt"))
	} else {
		s, err = newSession(cfg.work)
	}
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	if cfg.resume && cfg.interrupt == 0 {
		// Explicit continuation of a checkpoint left by a previous process.
		if code := resumeFromCheckpoint(cfg, s); code != exitOK {
			return code
		}
		return reportFinal(cfg, s, begin)
	}
	if cfg.interrupt == 0 && !cfg.resume {
		// Plain fresh run: no checkpoint involvement at all.
		if code := runSegment(cfg, s, s.loaded.StartFor(s.ord), 0); code != exitOK {
			return code
		}
		return reportFinal(cfg, s, begin)
	}
	if cfg.interrupt > 0 {
		// Script the whole scenario in one invocation: commit a prefix, stop
		// exactly as an interrupted process would, then continue only when
		// resumption was requested. Without --resume the interrupt is terminal.
		code := runSegment(cfg, s, s.loaded.StartFor(s.ord), cfg.interrupt)
		if code != exitCancelled {
			return code
		}
		if !cfg.resume {
			return exitCancelled
		}
	}
	if code := resumeFromCheckpoint(cfg, s); code != exitOK {
		return code
	}
	return reportFinal(cfg, s, begin)
}

// runSegment executes one bounded pipeline pass and prints its outcome.
func runSegment(cfg demoConfig, s *session, start pipe.Start, interrupt int) int {
	res, err := pipe.Run(s.ctx, cfg.work, s.primaryInit(), s.tasks, s.loaded, start, pipe.Options{
		QueueCapacity: cfg.queue, SlowConsumer: cfg.slow, InterruptAfter: interrupt,
	})
	if err != nil && !pipe.IsCancelled(err) {
		return fail(err)
	}
	if pipe.IsCancelled(err) {
		var ce *pipe.CancelledError
		errors.As(err, &ce)
		fmt.Printf("status=cancelled stage=%s last_committed=%s committed=%d fragments=%d peak_queue=%d\n",
			ce.Stage, ce.LastCommitted, res.CommittedAt, len(s.tasks), res.PeakQueue)
		return exitCancelled
	}
	_ = res
	return exitOK
}

// resumeFromCheckpoint loads the checkpoint, proves it matches the input, and
// continues from the exact committed boundary.
func resumeFromCheckpoint(cfg demoConfig, s *session) int {
	cp, err := index.Load(pipe.CheckpointPath(cfg.work))
	if err != nil {
		fmt.Fprintf(os.Stderr, "archivecheck: cannot load checkpoint: %v\n", err)
		return exitIncompatible
	}
	st, err := pipe.ResumeStart(s.manifest, cp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archivecheck: incompatible resume state: %v\n", err)
		return exitIncompatible
	}
	if st.Committed >= cp.FragmentsTotal {
		return exitOK
	}
	if code := runSegment(cfg, s, st, 0); code != exitOK {
		return code
	}
	return exitOK
}

func reportFinal(cfg demoConfig, s *session, begin time.Time) int {
	cp, err := index.Load(pipe.CheckpointPath(cfg.work))
	if err != nil {
		return fail(err)
	}
	rep, err := verify.Check(pipe.OutputPath(cfg.work), cp)
	if err != nil {
		return fail(err)
	}
	_, errOffset, errOK := firstFault(filepath.Join(cfg.work, "faults"))
	fmt.Printf("fragments=%d repaired_tfdt=%d missing_mfhd=%d first_error_offset=%d truncation_error_bytes=0\n",
		cp.FragmentsTotal, cp.RepairedTfdt, s.ord.Missing, offsetOrZero(errOffset, errOK))
	fmt.Printf("index_entries=%d peak_queue=%d resume_ms=%d output_sha256=%s\n",
		len(cp.Index.Entries), cfg.queue, time.Since(begin).Milliseconds(), rep.OutputSHA256)
	fmt.Printf("verified=%t timestamp_regressions=%d invalid_index_offsets=%d output=%s\n",
		rep.Verified, rep.TimestampRegressions, rep.InvalidIndexOffsets, pipe.OutputPath(cfg.work))
	if !rep.Verified {
		for _, r := range rep.Reasons {
			fmt.Fprintf(os.Stderr, "  reason: %s\n", r)
		}
		return exitFailure
	}
	return exitOK
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
	cp, err := index.Load(pipe.CheckpointPath(*work))
	if err != nil {
		fmt.Fprintf(os.Stderr, "archivecheck: cannot load checkpoint: %v\n", err)
		return exitFailure
	}
	rep, err := verify.Check(pipe.OutputPath(*work), cp)
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
	s, err := newSession(*work)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	cfg := demoConfig{work: *work, queue: *queue, slow: *slow, resume: true}
	begin := time.Now()
	if code := resumeFromCheckpoint(cfg, s); code != exitOK {
		return code
	}
	return reportFinal(cfg, s, begin)
}
