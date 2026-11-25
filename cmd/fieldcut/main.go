// Command fieldcut is the terminal acceptance adapter for the festival
// post-production pipeline.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"fieldcut/internal/clock"
	"fieldcut/internal/manifest"
	"fieldcut/internal/pipeline"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 3
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "resume":
		return runResume(args[1:])
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		return 3
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: fieldcut demo --case clean|damaged|interrupted --seed N --out DIR")
	fmt.Fprintln(os.Stderr, "       fieldcut verify --out DIR")
	fmt.Fprintln(os.Stderr, "       fieldcut resume --out DIR")
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	caseName := fs.String("case", "clean", "clean|damaged|interrupted")
	seed := fs.Int64("seed", 17, "deterministic fixture seed")
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "error_code=config_error message=--out is required")
		return 3
	}
	req := pipeline.Request{Case: *caseName, Seed: *seed, OutDir: *out}
	if *caseName == "interrupted" {
		req.Interrupt = true
	}
	res, err := pipeline.Run(req)
	return reportRun(res, err)
}

func runResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "error_code=config_error message=--out is required")
		return 3
	}
	res, err := pipeline.Resume(*out)
	return reportRun(res, err)
}

func reportRun(res *pipeline.Result, err error) int {
	if err != nil {
		if errors.Is(err, pipeline.ErrSimulatedInterrupt) {
			fmt.Println("simulated_interrupt=package_committed")
			if res != nil {
				fmt.Printf("reused_nodes=%d\n", len(res.ReusedNodes))
			}
			return 75
		}
		code := clock.CodeWrite
		track := -1
		if ce, ok := clock.AsError(err); ok {
			code = ce.Code
			track = ce.TrackID
		}
		fmt.Printf("error_code=%s track_id=%d message=%q\n", code, track, err.Error())
		if code == clock.CodeConfig || code == clock.CodeWrite || code == clock.CodeInvalidArgument {
			return 3
		}
		return 1
	}
	if res == nil {
		fmt.Println("error_code=empty_result")
		return 1
	}
	if res.Code == 2 && res.Manifest != nil {
		for _, line := range pipeline.MetricLines(res.Manifest) {
			fmt.Println(line)
		}
		return 2
	}
	if res.Manifest != nil {
		for _, line := range pipeline.MetricLines(res.Manifest) {
			fmt.Println(line)
		}
	}
	return res.Code
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "error_code=config_error message=--out is required")
		return 3
	}
	m, err := manifest.Verify(*out)
	if err != nil {
		fmt.Printf("verify=failed error_code=%s message=%q\n", errorCode(err), err.Error())
		return 2
	}
	fmt.Println("verify=ok")
	fmt.Printf("segment_count=%d\n", m.Metrics.SegmentCount)
	fmt.Printf("manifest_max_skew_ns=%d\n", m.Metrics.ManifestMaxSkewNs)
	fmt.Printf("bitrate_delta_pct=%.3f\n", m.Metrics.BitrateDeltaPct)
	return 0
}

func errorCode(err error) clock.ErrorCode {
	if ce, ok := clock.AsError(err); ok {
		return ce.Code
	}
	return clock.CodeBadInterval
}
