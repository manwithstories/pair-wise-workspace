// mediarun is the terminal demo adapter for the regional live-edge
// delivery pipeline. It generates sample data in code, runs the
// EBML -> timeline -> render -> manifest pipeline, and prints
// machine-checkable status lines.
//
// Exit codes: 0 success, 1 parameter/storage error, 2 rejected input.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"mediarun/internal/ebml"
	"mediarun/internal/fixture"
	"mediarun/internal/pipeline"
	"mediarun/internal/report"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mediarun <demo|verify> [flags]")
		return 1
	}
	switch args[0] {
	case "demo":
		return demo(args[1:])
	case "verify":
		return verify(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", args[0])
		return 1
	}
}

func demo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 1, "deterministic sample seed")
	duration := fs.Duration("duration", 12*time.Second, "sample duration")
	targetBps := fs.Int64("target-bitrate", 1_800_000, "target bitrate in bps")
	checkpoint := fs.String("checkpoint", "", "checkpoint file path")
	cache := fs.String("cache", "", "cache directory")
	out := fs.String("out", "", "output directory")
	interruptAfter := fs.Int("interrupt-after", 0, "stop after committing N segments")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *checkpoint == "" || *cache == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "error: --checkpoint, --cache and --out are required")
		return 1
	}

	durNs := duration.Nanoseconds()
	src, tracks := fixture.BuildEBML(*seed, durNs, *targetBps)
	var tm []report.TrackMap
	for _, t := range tracks {
		tm = append(tm, report.TrackMap{ID: t.ID, Kind: t.Kind})
	}
	in := pipeline.Input{
		Source: src,
		Tracks: tm,
		Packets: fixture.Packetize(src, fixture.RTPOptions{
			Duplicates: true, Reorder: true, WrapStart: true,
		}),
	}
	opts := pipeline.Options{
		Seed:           *seed,
		DurationNs:     durNs,
		TargetBps:      *targetBps,
		Fps:            2,
		SegmentNs:      2_000_000_000,
		OutDir:         *out,
		CacheDir:       *cache,
		CheckpointPath: *checkpoint,
		InterruptAfter: *interruptAfter,
	}
	res, err := pipeline.Run(opts, in)
	if err != nil {
		return reportError(err)
	}
	if res.Interrupted {
		fmt.Printf("status=interrupted checkpoint_end_ms=%d\n", res.CheckpointEndMs)
		return 0
	}
	if res.Resumed {
		fmt.Printf("status=resumed reused_segments=%d duplicate_commits=%d\n",
			res.ReusedSegments, res.DuplicateCommits)
	}
	m := res.Manifest
	fmt.Printf("status=ok source_sha256=%s tracks=%d frames=%d target_bps=%d actual_bps=%d "+
		"bitrate_delta_pct=%.2f quality_psnr_db=%.1f manifest_alignment_ms=%.1f\n",
		m.SourceSHA256, len(m.Tracks), len(m.Frames), m.TargetBps, m.ActualBps,
		m.BitrateDeltaPct, m.QualityPSNRdB, m.AlignmentMs)
	return 0
}

func verify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	out := fs.String("out", "", "output directory to verify")
	fixtureName := fs.String("fixture", "", "named fault fixture to verify against")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	switch *fixtureName {
	case "corrupted":
		// Truncated EBML must be rejected with code and offset.
		_, err := ebml.Parse(fixture.Corrupted())
		var ee *ebml.Error
		if errors.As(err, &ee) {
			fmt.Printf("status=rejected error_code=%s offset=%d\n", ee.Code, ee.Offset)
			return 2
		}
		fmt.Println("status=ok fixture=corrupted unexpectedly parsed")
		return 1
	case "":
		// Verify an existing output directory.
	default:
		fmt.Fprintln(os.Stderr, "error: unknown fixture", *fixtureName)
		return 1
	}

	if *out == "" {
		fmt.Fprintln(os.Stderr, "error: --out is required")
		return 1
	}
	m, err := report.Load(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := report.VerifySidecar(*out, m); err != nil {
		fmt.Printf("status=rejected error_code=SIDECAR_MISMATCH offset=0\n")
		return 2
	}
	for _, fr := range m.Frames {
		p, err := report.CheckPathInside(*out, fmt.Sprintf("frames/frame_%04d.raw", fr.Index))
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		data, err := os.ReadFile(p)
		if err != nil || pipeline.SHA256Of(data) != fr.SHA256 {
			fmt.Printf("status=rejected error_code=FRAME_DIGEST_MISMATCH offset=%d\n", fr.Index)
			return 2
		}
	}
	fmt.Printf("status=ok source_sha256=%s frames=%d\n", m.SourceSHA256, len(m.Frames))
	return 0
}

func reportError(err error) int {
	var perr *pipeline.Error
	if errors.As(err, &perr) {
		fmt.Printf("status=rejected error_code=%s offset=%d\n", perr.Code, perr.Offset)
		return 2
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}
