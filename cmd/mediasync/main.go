// Command mediasync runs the end-to-end media synchronisation demo and
// exposes per-stage subcommands for inspection.
//
// Exit codes: 0 success, 2 recoverable input errors, 1 invalid arguments or
// unrecoverable input.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"mediasync/internal/media"
	"mediasync/internal/pipeline"
)

const usage = `mediasync - multi-camera media synchronisation toolkit

usage:
  mediasync demo [flags]        run the full generated pipeline
  mediasync inspect [flags]     parse caller-supplied subtitle/MP4 files
  mediasync version             print the build version

flags for demo:
  --seed N                 deterministic fixture seed (default 42)
  --packet-loss-burst N    packets dropped per injected burst (default 4)
  --bandwidth-kbps A,B,C   simulated uplink profile in kbps (default 800,2400,600)
  --frames RATE            camera A frame rate (default 25)
  --target-ms N            target clip length (default 1500)
  --overlap-ms N           clip overlap (default 200)
  --window N               RTP reorder window in packets (default 64)
  --out PATH               write the JSON report (atomic, traversal-checked)
` + inspectUsage

const inspectUsage = `
flags for inspect:
  --subtitle a.srt,b.vtt   parse subtitles and report issues with offsets
  --mp4 a.mp4,b.mp4        build sample/keyframe indexes and report issues
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is separated from main so tests can drive the CLI in-process.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 1
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "inspect":
		return runInspect(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "mediasync 1.0.0")
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "mediasync: unknown command %q\n\n%s", args[0], usage)
		return 1
	}
}

func runDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		seed      = fs.Int64("seed", 42, "deterministic fixture seed")
		burst     = fs.Int("packet-loss-burst", 4, "packets dropped per injected burst")
		bandwidth = fs.String("bandwidth-kbps", "800,2400,600", "uplink profile in kbps")
		frames    = fs.String("frames", "25", "camera A frame rate")
		targetMS  = fs.Int64("target-ms", 1500, "target clip length in milliseconds")
		overlapMS = fs.Int64("overlap-ms", 200, "clip overlap in milliseconds")
		window    = fs.Int("window", 64, "RTP reorder window in packets")
		out       = fs.String("out", "", "write JSON report to this path")
		quiet     = fs.Bool("quiet", false, "suppress the human-readable summary")
	)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "mediasync: unexpected argument %q\n", fs.Arg(0))
		return 1
	}
	if *seed < 0 {
		fmt.Fprintln(stderr, "mediasync: --seed must be non-negative")
		return 1
	}
	if *burst < 0 || *burst > 64 {
		fmt.Fprintln(stderr, "mediasync: --packet-loss-burst must be within 0..64")
		return 1
	}
	if *targetMS <= 0 || *targetMS > 600_000 {
		fmt.Fprintln(stderr, "mediasync: --target-ms must be within 1..600000")
		return 1
	}
	if *overlapMS < 0 || *overlapMS >= *targetMS {
		fmt.Fprintln(stderr, "mediasync: --overlap-ms must be within 0..target-ms")
		return 1
	}
	if *window <= 0 || *window > 1<<15 {
		fmt.Fprintln(stderr, "mediasync: --window must be within 1..32768")
		return 1
	}
	kbps, err := parseBandwidth(*bandwidth)
	if err != nil {
		fmt.Fprintf(stderr, "mediasync: %v\n", err)
		return 1
	}

	opts := pipeline.DefaultOptions()
	opts.Seed = *seed
	opts.PacketLoss = *burst
	opts.BandwidthKbps = kbps
	opts.Frames = *frames
	opts.TargetMS = *targetMS
	opts.OverlapMS = *overlapMS
	opts.Window = *window
	opts.Expected = map[media.Code]bool{
		media.CodeOutOfRange:       true,
		media.CodeMissingEntry:     true,
		media.CodeInvalidTimestamp: true,
	}

	report, err := pipeline.Run(opts)
	if err != nil {
		fmt.Fprintf(stderr, "mediasync: %v\n", err)
		return 1
	}
	report.GeneratedAt = ""
	if *out != "" {
		if err := report.WriteJSON(*out); err != nil {
			fmt.Fprintf(stderr, "mediasync: write report: %v\n", err)
			return 1
		}
	}
	if !*quiet {
		fmt.Fprint(stdout, report.Summary())
	}
	return report.ExitCode()
}

func parseBandwidth(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("--bandwidth-kbps: %q is not an integer", p)
		}
		if v < 64 || v > 1_000_000 {
			return nil, fmt.Errorf("--bandwidth-kbps: %d is outside 64..1000000", v)
		}
		out = append(out, v)
	}
	return out, nil
}
