// Command relay is the terminal acceptance and demonstration entry point.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pairwise/relay/internal/checkpoint"
	"github.com/pairwise/relay/internal/fixture"
	"github.com/pairwise/relay/internal/media"
	"github.com/pairwise/relay/internal/pipeline"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		code := media.ExitCodeOf(err)
		if code == 1 {
			code = 3
		}
		os.Exit(code)
	}
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	work := fs.String("work", "", "working directory for cache, checkpoint and manifests")
	seed := fs.Int64("seed", 17, "deterministic fixture seed")
	rungs := fs.Int("rungs", 3, "number of representations")
	segments := fs.Int("segments", 24, "segments per representation")
	faultSuite := fs.Bool("fault-suite", false, "generate all acceptance fault samples")
	interruptAfter := fs.Int("interrupt-after", 0, "simulate interruption after this common segment")
	resume := fs.Bool("resume", false, "resume from checkpoint after simulated interruption")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return media.NewExitError(2, err)
	}
	if *work == "" {
		return media.NewExitError(2, errors.New("--work is required"))
	}

	cfg := fixture.Config{Seed: *seed, Rungs: *rungs, Segments: *segments, FaultSuite: *faultSuite}
	full := fixture.Generate(cfg)
	ctx := context.Background()

	if *interruptAfter > 0 {
		fresh := full
		fresh.Resume = false
		fresh.InterruptAfter = *interruptAfter
		stopped, err := pipeline.Run(ctx, fresh, *work)
		if err != nil {
			return err
		}
		if !stopped.Interrupted || stopped.Checkpoint.ProcessedSegments != *interruptAfter {
			return media.NewExitError(3, fmt.Errorf("interruption did not land after segment %d", *interruptAfter))
		}
		fmt.Printf("interrupted_after=%d checkpoint=%s\n", *interruptAfter, filepath.Join(*work, "checkpoint.json"))
	}

	workInput := full
	var resumeMS int64
	var primary *pipeline.Result
	if *resume {
		workInput.Resume = true
		start := time.Now()
		r, err := pipeline.Run(ctx, workInput, *work)
		resumeMS = time.Since(start).Milliseconds()
		if err != nil {
			return err
		}
		primary = r
	} else {
		r, err := pipeline.Run(ctx, full, *work)
		if err != nil {
			return err
		}
		primary = r
	}

	fullRef := full
	fullRef.Resume = false
	fullResult, err := pipeline.Run(ctx, fullRef, filepath.Join(*work, "full-reference"))
	if err != nil {
		return err
	}

	rerunInput := full
	rerunInput.Resume = true
	rerunResult, err := pipeline.Run(ctx, rerunInput, *work)
	if err != nil {
		return err
	}
	verified, err := pipeline.Verify(ctx, full, *work)
	if err != nil {
		return err
	}

	rerunEqual := primary.ArtifactSHA256 == fullResult.ArtifactSHA256 &&
		primary.ArtifactSHA256 == rerunResult.ArtifactSHA256 &&
		primary.ArtifactSHA256 == verified.ArtifactSHA256
	if !rerunEqual {
		return media.NewExitError(4, media.NewFault(media.CodeArtifactMismatch, media.FragmentID{}, -1, false,
			"artifact digests differ: resumed=%s full=%s rerun=%s verified=%s",
			primary.ArtifactSHA256, fullResult.ArtifactSHA256, rerunResult.ArtifactSHA256, verified.ArtifactSHA256))
	}
	if primary.DamageOffsetDelta != 0 {
		return media.NewExitError(3, fmt.Errorf("damage offset delta=%d", primary.DamageOffsetDelta))
	}

	fmt.Printf("status=ok\n")
	fmt.Printf("segments=%d\n", primary.Segments)
	fmt.Printf("renditions=%d\n", primary.Renditions)
	fmt.Printf("aligned=%d/%d\n", primary.Aligned, primary.Segments)
	fmt.Printf("transport_duplicates=%d out_of_order=%d recovered_packets=%d closed_gaps=%d\n",
		primary.TransportStats.Duplicates, primary.TransportStats.OutOfOrder,
		primary.TransportStats.Recovered, primary.TransportStats.Missing)
	fmt.Printf("mfhd_gap_events=%d tfdt_repaired=%d discontinuities=%d\n",
		primary.MFHDGaps, primary.TFDTRepaired, len(primary.Discontinuities))
	fmt.Printf("damage_checked=%d\n", primary.DamageChecked)
	fmt.Printf("damage_offset_delta=%d\n", primary.DamageOffsetDelta)
	fmt.Printf("resume_ms=%d\n", resumeMS)
	fmt.Printf("resume_within_target=%t\n", resumeMS < 2000)
	fmt.Printf("cache_rehash=%d\n", verified.CacheStats.Rehashes)
	fmt.Printf("cache_hits=%d\n", verified.CacheStats.Hits)
	fmt.Printf("artifact_sha256=%s\n", primary.ArtifactSHA256)
	fmt.Printf("rerun_equal=%t\n", rerunEqual)
	if len(primary.GapSegments) > 0 {
		fmt.Printf("gap_segments=")
		for i, g := range primary.GapSegments {
			if i > 0 {
				fmt.Print(",")
			}
			fmt.Printf("%d", g)
		}
		fmt.Println()
	}
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	work := fs.String("work", "", "demo working directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return media.NewExitError(2, err)
	}
	if *work == "" {
		return media.NewExitError(2, errors.New("--work is required"))
	}
	cp, err := checkpoint.Load(filepath.Join(*work, "checkpoint.json"))
	if err != nil {
		return err
	}
	ctx := context.Background()
	in := fixture.Generate(fixture.Config{Seed: cp.Seed, Rungs: cp.Rungs, Segments: cp.Segments, FaultSuite: true})
	res, err := pipeline.Verify(ctx, in, *work)
	if err != nil {
		// A checkpoint from a non-fault-suite run has the alternate digest.
		plain := fixture.Generate(fixture.Config{Seed: cp.Seed, Rungs: cp.Rungs, Segments: cp.Segments, FaultSuite: false})
		var err2 error
		res, err2 = pipeline.Verify(ctx, plain, *work)
		if err2 != nil {
			return err
		}
	}
	fmt.Printf("verify=ok\n")
	fmt.Printf("segments=%d\n", res.Segments)
	fmt.Printf("renditions=%d\n", res.Renditions)
	fmt.Printf("artifact_sha256=%s\n", res.ArtifactSHA256)
	fmt.Printf("cache_rehash=%d\n", res.CacheStats.Rehashes)
	fmt.Printf("rerun_equal=true\n")
	return nil
}

func usage(w interface{ Write([]byte) (int, error) }) {
	fmt.Fprintln(w, "usage: relay <demo|verify> [flags]")
	fmt.Fprintln(w, "  demo --work DIR --seed N --rungs N --segments N --fault-suite --interrupt-after N --resume")
	fmt.Fprintln(w, "  verify --work DIR")
}
