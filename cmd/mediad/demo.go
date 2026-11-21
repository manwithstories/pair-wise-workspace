package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/model"
	"mediad/internal/pipeline"
	"mediad/internal/report"
	"mediad/internal/txn"
)

func runDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Int64("seed", 17, "random seed")
	faults := fs.String("faults", "", "comma list: duplicate,reorder,burst-loss,corrupt-log")
	bw := fs.String("bandwidth-mbps", "", "comma list of bandwidths in Mbps")
	interrupt := fs.String("interrupt-after", "", "stage after which to simulate a crash")
	resume := fs.Bool("resume", false, "reuse checksum-verified stages after a crash")
	dir := fs.String("dir", "", "working directory")
	seeks := fs.Int("seeks", 11, "index probes per track")
	maxBurst := fs.Int("max-burst", 8, "longest recoverable packet gap")
	asJSON := fs.Bool("json", false, "print only the JSON summary")
	maxAlignMS := fs.Float64("max-alignment-ms", 1.0, "alignment gate in milliseconds")
	maxDeltaPct := fs.Float64("max-bitrate-delta-pct", 5.0, "bitrate deviation gate in percent")
	minPSNR := fs.Float64("min-psnr", 30.0, "minimum quality gate in dB")
	if err := fs.Parse(args); err != nil {
		return exitInvalid
	}

	faultList, err := parseFaults(*faults)
	if err != nil {
		fmt.Fprintf(stderr, "mediad: %v\n", err)
		return exitInvalid
	}
	bandwidths, err := parseFloats(*bw)
	if err != nil {
		fmt.Fprintf(stderr, "mediad: --bandwidth-mbps: %v\n", err)
		return exitInvalid
	}
	var stage txn.Stage
	if *interrupt != "" {
		stage = txn.Stage(*interrupt)
		valid := false
		for _, s := range txn.Order {
			if s == stage {
				valid = true
			}
		}
		if !valid {
			fmt.Fprintf(stderr, "mediad: --interrupt-after: unknown stage %q\n", *interrupt)
			return exitInvalid
		}
	}
	if *resume && stage == "" {
		// Resume without an explicit interruption point still recovers from
		// whatever the log contains, which is how a killed process restarts.
		if *dir == "" {
			fmt.Fprintf(stderr, "mediad: --resume without --dir has no log to resume from\n")
			return exitInvalid
		}
	}

	workDir := *dir
	if workDir == "" {
		workDir = filepath.Join(os.TempDir(), fmt.Sprintf("mediad-demo-%d", *seed))
	}
	opt := pipeline.DemoOptions{
		Seed: *seed, Faults: faultList, BandwidthMbps: bandwidths,
		InterruptAfter: stage, Resume: *resume, Dir: workDir,
		SeeksPerTrack: *seeks, MaxBurst: *maxBurst,
	}
	res, err := pipeline.Run(opt)
	if err != nil {
		fmt.Fprintf(stderr, "mediad: %v\n", err)
		code := model.ExitCodeOf(err)
		if code == 0 {
			code = exitFailure
		}
		return code
	}

	// Verification gates can be tightened for inspection; the defaults are
	// the documented acceptance thresholds.
	gateErr := res.Manifest.Gate(*maxAlignMS, *maxDeltaPct)
	if gateErr == nil {
		gateErr = report.Gate(res.Summary, *minPSNR)
	}

	if *asJSON {
		data, err := res.Summary.JSON()
		if err != nil {
			fmt.Fprintf(stderr, "mediad: %v\n", err)
			return exitFailure
		}
		fmt.Fprintf(stdout, "%s\n", data)
	} else {
		printReport(stdout, res)
	}
	if gateErr != nil {
		fmt.Fprintf(stderr, "mediad: verification failed: %v\n", gateErr)
		if gateErrKO(gateErr) {
			return exitThreshold
		}
		return model.ExitCodeOf(gateErr)
	}
	return exitOK
}

// runFixtures writes the same MP4 and MXF samples the demo uses, so the
// probe can be exercised on a file outside a pipeline run. With --bad-crop it
// also writes a sample whose clean aperture exceeds the frame, which the
// probe must reject.
func runFixtures(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fixtures", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Int64("seed", 17, "fixture seed")
	badCrop := fs.Bool("bad-crop", false, "also write an out-of-range crop sample")
	if err := fs.Parse(args); err != nil {
		return exitInvalid
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "mediad: fixtures takes exactly one directory")
		return exitInvalid
	}
	dir := fs.Arg(0)
	if err := pipeline.WriteFixtures(*seed, dir); err != nil {
		fmt.Fprintf(stderr, "mediad: %v\n", err)
		return model.ExitCodeOf(err)
	}
	fmt.Fprintf(stdout, "wrote %s and %s\n", filepath.Join(dir, pipeline.MP4Name), filepath.Join(dir, pipeline.MXFName))
	if *badCrop {
		path := filepath.Join(dir, "bad-crop.mp4")
		if err := pipeline.WriteBadCropFixture(*seed, path); err != nil {
			fmt.Fprintf(stderr, "mediad: %v\n", err)
			return model.ExitCodeOf(err)
		}
		fmt.Fprintf(stdout, "wrote %s\n", path)
	}
	return exitOK
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func gateErrKO(err error) bool {
	return model.KindOf(err) == model.KindThreshold
}
