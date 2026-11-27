// Command relayaudit generates a seeded four-track relay segment, drives it
// through the audit pipeline and prints a summary suitable for terminal use.
//
// Exit codes:
//
//	0  every check passed
//	2  invalid command line
//	3  input unrecoverable
//	4  output verification failed
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"relayaudit/internal/checkpoint"
	"relayaudit/internal/fault"
	"relayaudit/internal/fixture"
	"relayaudit/internal/pipeline"
	"relayaudit/internal/verify"
)

const (
	exitOK            = 0
	exitUsage         = 2
	exitUnrecoverable = 3
	exitVerify        = 4
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitUsage
	}
	switch args[0] {
	case "selftest":
		return selftest(args[1:])
	case "-h", "--help", "help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "relayaudit: unknown command %q\n\n", args[0])
		usage()
		return exitUsage
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `relayaudit — relay stream audit kernel

usage:
  relayaudit selftest [flags]    generate a fixture, audit it and print a summary

selftest flags:
  --seed N              fixture seed (default 73)
  --size BYTES          fixture size budget (default 8388608)
  --truncate-tail N     cut N bytes from the end
  --corrupt-record N    flip one payload byte in record N (1-based)
  --drop-index-page N   blank index page N (1-based)
  --interrupt-after N   stop after N records, commit a checkpoint, then resume
  --resume              continue from an existing checkpoint instead of resetting
  --dir PATH            working directory for checkpoints (default: .relayaudit)
`)
}

type options struct {
	seed           int64
	size           int
	truncateTail   int
	corruptRecord  int
	dropIndexPage  int
	interruptAfter int
	resume         bool
	dir            string
}

func parseOptions(name string, args []string) (*options, error) {
	o := &options{seed: 73, size: 8 << 20, dir: ".relayaudit"}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Int64Var(&o.seed, "seed", o.seed, "fixture seed")
	fs.IntVar(&o.size, "size", o.size, "fixture size budget in bytes")
	fs.IntVar(&o.truncateTail, "truncate-tail", 0, "truncate this many bytes from the end")
	fs.IntVar(&o.corruptRecord, "corrupt-record", 0, "corrupt this 1-based record")
	fs.IntVar(&o.dropIndexPage, "drop-index-page", 0, "blank this 1-based index page")
	fs.IntVar(&o.interruptAfter, "interrupt-after", 0, "interrupt after this many records")
	fs.BoolVar(&o.resume, "resume", false, "resume from an existing checkpoint")
	fs.StringVar(&o.dir, "dir", o.dir, "checkpoint working directory")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	switch {
	case o.size < 1<<16:
		return nil, fmt.Errorf("--size must be at least %d", 1<<16)
	case o.truncateTail < 0:
		return nil, fmt.Errorf("--truncate-tail must not be negative")
	case o.corruptRecord < 0, o.dropIndexPage < 0, o.interruptAfter < 0:
		return nil, fmt.Errorf("record, page and interrupt counts must not be negative")
	}
	return o, nil
}

func generate(o *options) (*fixture.Segment, error) {
	return fixture.Generate(fixture.Spec{
		Seed: o.seed, Size: o.size,
		TruncateTail:  o.truncateTail,
		CorruptRecord: o.corruptRecord,
		DropIndexPage: o.dropIndexPage,
	})
}

func newStore(o *options) (*checkpoint.Store, error) {
	return checkpoint.NewStore(filepath.Join(o.dir, "ckpt"))
}

func selftest(args []string) int {
	o, err := parseOptions("selftest", args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relayaudit: %v\n", err)
		return exitUsage
	}
	seg, err := generate(o)
	if err != nil {
		// Generation only fails on an impossible parameter combination, so
		// it is a usage problem rather than an unrecoverable input.
		fmt.Fprintf(os.Stderr, "relayaudit: %v\n", err)
		return exitUsage
	}
	store, err := newStore(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relayaudit: %v\n", err)
		return exitUnrecoverable
	}
	// A plain run starts from a clean store. A run that is asked to
	// interrupt itself must also start clean, otherwise a previous finished
	// checkpoint would short-circuit the demo and hide the resume path.
	if !o.resume || o.interruptAfter > 0 {
		if err := store.Reset(); err != nil {
			fmt.Fprintf(os.Stderr, "relayaudit: %v\n", err)
			return exitUnrecoverable
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --interrupt-after deliberately stops mid-run and then resumes, so the
	// selftest exercises both paths in one invocation. --resume on its own
	// continues whatever the previous invocation left behind.
	interruptAfter := o.interruptAfter
	start := time.Now()
	out, err := pipeline.Run(ctx, pipeline.Config{
		Source: seg.Bytes, Seed: o.seed, Store: store,
		InterruptAfter: interruptAfter, Resume: o.resume,
	})
	if errors.Is(err, pipeline.ErrInterrupted) && out != nil && out.Interrupted {
		fmt.Printf("interrupted after %d records; checkpoint committed, resuming\n", out.NextIndex)
		resumed, rerr := pipeline.Run(ctx, pipeline.Config{
			Source: seg.Bytes, Seed: o.seed, Store: store, Resume: true,
		})
		if rerr != nil {
			return reportFailure(o, seg, out, rerr)
		}
		out = resumed
	} else if err != nil {
		return reportFailure(o, seg, out, err)
	}

	res := out.Timeline
	if res == nil {
		fmt.Fprintf(os.Stderr, "relayaudit: verify setup: timeline missing\n")
		return exitUnrecoverable
	}
	result := verify.Check(out.Output, out.Scan, res, out.Ranges, out.OutputSHA256, verify.Expected{
		CorruptOffset:   seg.Damage.CorruptRecordOff,
		CorruptInjected: seg.Damage.CorruptRecord != 0,
		TruncateTail:    seg.Damage.TruncateTail,
		DamageRanges:    expectedRanges(seg),
		TrackCount:      seg.TrackCount,
		SourceSize:      int64(len(seg.Bytes)),
	})
	result.ResumeMillis = out.ResumeMillis
	if out.ResumeMillis > 2000 {
		result.Problems = append(result.Problems,
			fmt.Sprintf("resume took %d ms, exceeding the 2000 ms budget", out.ResumeMillis))
	}
	if out.OutputSHA256 == "" || len(out.OutputSHA256) != 64 {
		result.Problems = append(result.Problems, "output digest is not 64 hex characters")
	}
	if len(result.Problems) > 0 {
		result.ExitCode = exitVerify
	}

	printSummary(o, seg, out, result, time.Since(start))
	if len(result.Problems) > 0 {
		for _, p := range result.Problems {
			fmt.Fprintf(os.Stderr, "check failed: %s\n", p)
		}
		return exitVerify
	}
	return exitOK
}

// expectedRanges counts the independent damaged spans for this fixture.
func expectedRanges(seg *fixture.Segment) int {
	n := 0
	if seg.Damage.CorruptRecord > 0 {
		n++
	}
	if seg.Damage.TruncateTail > 0 {
		n++
	}
	return n
}

func reportFailure(o *options, seg *fixture.Segment, out *pipeline.Outcome, err error) int {
	fmt.Fprintf(os.Stderr, "relayaudit: %v\n", err)
	switch {
	case fault.IsKind(err, fault.Canceled):
		fmt.Fprintln(os.Stderr, "run canceled; valid checkpoint preserved")
		return exitUnrecoverable
	case fault.IsKind(err, fault.Checkpoint):
		// A checkpoint problem is recoverable by clearing state, but the
		// current input cannot be trusted for this run.
		return exitUnrecoverable
	case fault.IsKind(err, fault.Verify):
		return exitVerify
	case fault.IsKind(err, fault.Truncated), fault.IsKind(err, fault.Length), fault.IsKind(err, fault.Limit):
		return exitUnrecoverable
	default:
		return exitUnrecoverable
	}
}
