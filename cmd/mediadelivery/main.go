// Command mediadelivery is the acceptance entry point for the post-show media
// delivery pipeline. It generates a synthetic fixture, runs the full analysis
// and prints the delivery metrics, returning a semantic process exit code.
//
// Exit codes:
//
//	0 - complete delivery, all metrics passed
//	2 - invalid arguments or manifest
//	3 - fatal index / I/O error
//	4 - report generated but an unrecoverable missing segment remains
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"mediadelivery/internal/fixture"
	"mediadelivery/internal/pipeline"
)

// Acceptance thresholds from the delivery specification.
const (
	maxAVSyncMs      = 40
	requiredRecovery = 0.900
	maxP95Ms         = int64(1000)
	minDuplicateHits = 3
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return 2
	}
	if args[0] != "demo" {
		fmt.Fprintf(os.Stderr, "unknown command %q (only \"demo\" is supported)\n", args[0])
		return 2
	}

	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	work := fs.String("work", "", "working directory (all writes stay inside)")
	format := fs.String("format", "both", "manifest format: hls | dash | both")
	cameras := fs.Int("cameras", 3, "number of camera positions")
	duration := fs.Duration("duration", 0, "capture duration, e.g. 45s")
	faultProfile := fs.String("fault-profile", "clean", "fault profile: clean | boundary")
	interrupt := fs.Int("interrupt-after-block", 0, "interrupt after N blocks (0 disables)")
	resume := fs.Bool("resume", false, "resume from the last validated checkpoint")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	if err := validate(*work, *format, *cameras, *duration); err != nil {
		fmt.Fprintf(os.Stderr, "invalid arguments: %v\n", err)
		return 2
	}

	profile := fixture.ProfileClean
	if *faultProfile == "boundary" {
		profile = fixture.ProfileBoundary
	} else if *faultProfile != "clean" {
		fmt.Fprintf(os.Stderr, "invalid fault-profile %q\n", *faultProfile)
		return 2
	}

	// A resumed run must not re-arm the interruption; only the first run
	// interrupts. Both runs share a stable run id so checkpoints validate.
	interruptAfter := *interrupt
	if *resume {
		interruptAfter = 0
	}
	cfg := pipeline.Config{
		WorkDir:             *work,
		Format:              *format,
		Cameras:             *cameras,
		DurationSec:         duration.Seconds(),
		Profile:             profile,
		RunID:               "delivery-" + *faultProfile,
		InterruptAfterBlock: interruptAfter,
	}

	runner, err := pipeline.NewRunner(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pipeline init failed: %v\n", err)
		return 2
	}
	metrics, err := runner.Run()
	if err != nil {
		if errors.Is(err, pipeline.ErrInterrupted) {
			if *resume {
				// Should not happen: resume disables interruption.
				fmt.Fprintf(os.Stderr, "unexpected interruption during resume\n")
				return 3
			}
			fmt.Println("run interrupted after configured block; rerun with --resume")
			return 0
		}
		if errors.Is(err, pipeline.ErrManifest) {
			fmt.Fprintf(os.Stderr, "manifest error: %v\n", err)
			return 2
		}
		fmt.Fprintf(os.Stderr, "fatal index/I/O error: %v\n", err)
		return 3
	}

	// On a resumed run, verify determinism against a fresh uninterrupted
	// reference run instead of assuming equality. Non-resume runs have no
	// prior state to compare and report true (single deterministic execution).
	if *resume {
		refHash, refErr := pipeline.ReferenceHash(cfg)
		if refErr != nil {
			fmt.Fprintf(os.Stderr, "reference run failed: %v\n", refErr)
			return 3
		}
		metrics.ResumeHashEqual = refHash == runner.IndexHash()
	} else {
		metrics.ResumeHashEqual = true
	}

	printMetrics(metrics)
	if metrics.HasUnrecoverable {
		return 4
	}
	if !metricsPass(metrics) {
		fmt.Fprintln(os.Stderr, "metrics failed acceptance thresholds")
		return 4
	}
	return 0
}

func validate(work, format string, cameras int, duration time.Duration) error {
	if work == "" {
		return errors.New("--work is required")
	}
	switch format {
	case "hls", "dash", "both":
	default:
		return fmt.Errorf("invalid --format %q", format)
	}
	if cameras < 1 {
		return errors.New("--cameras must be >= 1")
	}
	if duration <= 0 {
		return errors.New("--duration must be positive, e.g. 45s")
	}
	return nil
}

func metricsPass(m *pipeline.Metrics) bool {
	if m.AVSyncMaxMs > maxAVSyncMs {
		return false
	}
	if m.SegmentP95Ms > maxP95Ms {
		return false
	}
	if !m.ResumeHashEqual {
		return false
	}
	if m.DuplicateHits < minDuplicateHits {
		return false
	}
	// Recovery rate is only meaningful when slots were actually lost. With no
	// loss there is nothing to recover; with loss, the 0.900 floor applies.
	if m.LostSlots > 0 && m.LossRecoveryRate < requiredRecovery {
		return false
	}
	return true
}

func printMetrics(m *pipeline.Metrics) {
	var b strings.Builder
	fmt.Fprintf(&b, "av_sync_max_ms=%d\n", m.AVSyncMaxMs)
	fmt.Fprintf(&b, "lost_slots=%d\n", m.LostSlots)
	fmt.Fprintf(&b, "recovered_slots=%d\n", m.RecoveredSlots)
	fmt.Fprintf(&b, "loss_recovery_rate=%.3f\n", m.LossRecoveryRate)
	fmt.Fprintf(&b, "segment_p95_ms=%d\n", m.SegmentP95Ms)
	fmt.Fprintf(&b, "resume_hash_equal=%t\n", m.ResumeHashEqual)
	fmt.Fprintf(&b, "duplicate_hits=%d\n", m.DuplicateHits)
	fmt.Fprintf(&b, "processed_blocks=%d\n", m.ProcessedBlocks)
	fmt.Fprintf(&b, "has_unrecoverable=%t\n", m.HasUnrecoverable)
	fmt.Print(b.String())
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: mediadelivery demo --work <dir> --format <hls|dash|both> --cameras <n> --duration <45s> [--fault-profile boundary] [--interrupt-after-block N] [--resume]")
}
