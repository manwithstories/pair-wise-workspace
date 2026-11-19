// Command mediamix runs the local media pipeline demo over generated fixtures.
//
// Exit codes:
//
//	0  completed
//	2  invalid arguments
//	3  input validation failed
//	4  processing finished with unrecoverable tracks
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"mediamix/internal/core"
	"mediamix/internal/pipeline"
)

const usageText = `mediamix - local multi-track proxy media pipeline

Usage:
  mediamix demo [flags]

Flags:
  --seed int             fixture seed (default 41)
  --loss-burst int       packets lost per burst (default 0)
  --bandwidth-mbps float uplink bandwidth sample (default 3)
  --corrupt-index        inject a bad keyframe index for one video track
  --truncate-track id    truncate a track's media (e.g. audio-2)
  --json                 emit the machine-readable report
  --help                 show this help
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is separated from main so tests can drive the CLI without exiting.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usageText)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usageText)
		return 2
	}
}

// demoFlags holds parsed demo arguments.
type demoFlags struct {
	seed          int64
	lossBurst     int
	bandwidthMBps float64
	corruptIndex  bool
	truncateTrack string
	json          bool
}

func runDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opts := demoFlags{}
	fs.Int64Var(&opts.seed, "seed", 41, "fixture seed")
	fs.IntVar(&opts.lossBurst, "loss-burst", 0, "packets lost per burst")
	fs.Float64Var(&opts.bandwidthMBps, "bandwidth-mbps", 3, "uplink bandwidth sample")
	fs.BoolVar(&opts.corruptIndex, "corrupt-index", false, "inject a bad keyframe index")
	fs.StringVar(&opts.truncateTrack, "truncate-track", "", "truncate a track's media")
	fs.BoolVar(&opts.json, "json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stderr, "bad arguments: %v\n", err)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if opts.lossBurst < 0 {
		fmt.Fprintln(stderr, "bad arguments: --loss-burst must be >= 0")
		return 2
	}
	if opts.bandwidthMBps <= 0 {
		fmt.Fprintln(stderr, "bad arguments: --bandwidth-mbps must be > 0")
		return 2
	}

	rep, err := pipeline.Run(pipeline.Options{
		Seed:          opts.seed,
		LossBurst:     opts.lossBurst,
		BandwidthMBps: opts.bandwidthMBps,
		CorruptIndex:  opts.corruptIndex,
		TruncateTrack: opts.truncateTrack,
	})
	if err != nil {
		// A generation or validation failure before processing is a parameter
		// or input problem; classify by its code rather than assuming.
		code := core.CodeOf(err)
		fmt.Fprintf(stderr, "validation failed: %v\n", err)
		if code == core.CodeBadArgument || code == core.CodeOverLimit {
			return 2
		}
		return 3
	}

	if opts.json {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "encode report: %v\n", err)
			return 3
		}
	} else {
		writeHuman(stdout, rep)
	}
	return rep.ExitCode
}

// writeHuman prints a compact terminal summary.
func writeHuman(w io.Writer, rep *pipeline.Report) {
	fmt.Fprintf(w, "seed=%d bandwidth=%.1fMbps buffer_bound=%d\n", rep.Seed, rep.BandwidthMBps, rep.BufferBound)
	fmt.Fprintf(w, "sync_error_ms=%.3f recovery_rate=%.3f latency_p95_ms=%.1f\n",
		rep.SyncErrorMS, rep.RecoveryRate, rep.LatencyP95MS)
	fmt.Fprintf(w, "reordered_packets=%d duplicate_packets=%d\n", rep.ReorderedPackets, rep.DuplicatePackets)
	fmt.Fprintf(w, "failed_tracks=%s\n", list(rep.FailedTracks))
	if len(rep.DegradedTracks) > 0 {
		fmt.Fprintf(w, "degraded_tracks=%s\n", list(rep.DegradedTracks))
	}
	for _, v := range rep.ValidationErrors {
		fmt.Fprintf(w, "index_error code=%s track=%s detail=%s\n", v.Code, v.Track, v.Detail)
	}
	for _, tr := range rep.Tracks {
		line := fmt.Sprintf("  %-11s kind=%-5s status=%-8s delivered=%d recovered=%d lost=%d p95=%.1fms",
			tr.Track, tr.Kind, tr.Status, tr.Delivered, tr.Recovered, tr.Unrecoverable, tr.LatencyP95Ms)
		if tr.Error != nil {
			line += " error=" + tr.Error.Code
		}
		for _, a := range tr.Adjustments {
			line += " [" + a.Code + "]"
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintf(w, "exit_code=%d\n", rep.ExitCode)
}

func list(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(items, ",")
}

// exitCodeFor is exposed for tests that assert the mapping.
func exitCodeFor(err error) int {
	switch core.CodeOf(err) {
	case core.CodeBadArgument, core.CodeOverLimit:
		return 2
	default:
		return 3
	}
}
