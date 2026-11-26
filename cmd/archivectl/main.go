// Command archivectl scans archival WAV files, builds digest-keyed indexes,
// repairs damaged PCM streams, and verifies frame shard hashes.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"archivectl/internal/core"
	"archivectl/internal/demo"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 1
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "scan":
		return runScan(args[1:])
	case "index":
		return runIndex(args[1:])
	case "repair":
		return runRepair(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "-h", "--help", "help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage(os.Stderr)
		return 1
	}
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	workDir := fs.String("workdir", "demo-work", "local directory for generated fixtures, index, and output")
	seed := fs.Int64("seed", 41, "deterministic fixture seed")
	frames := fs.Int("frames", 196608, "number of sample frames to generate")
	channels := fs.Int("channels", 6, "number of PCM channels (1..8)")
	inject := fs.String("inject", "wrap,drift,vfr,bad-crc,truncate,missing-cue", "comma-separated fixture injections")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	injections := splitCSV(*inject)
	rep, err := demo.Run(demo.Options{
		WorkDir: *workDir, Seed: *seed, Frames: *frames,
		Channels: *channels, Inject: injections,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo failed: %v\n", err)
		return core.ExitCode(err)
	}

	roundtripRate := 0.0
	if rep.RoundtripTotal > 0 {
		roundtripRate = float64(rep.RoundtripMatched) / float64(rep.RoundtripTotal)
	}
	keyframeRate := 0.0
	if rep.KeyframeTotal > 0 {
		keyframeRate = float64(rep.KeyframeHits) / float64(rep.KeyframeTotal)
	}
	fmt.Printf("workdir=%s\n", rep.WorkDir)
	fmt.Printf("roundtrip_match=%d/%d\n", rep.RoundtripMatched, rep.RoundtripTotal)
	fmt.Printf("roundtrip_rate=%.3f\n", roundtripRate)
	fmt.Printf("keyframe_hits=%d/%d\n", rep.KeyframeHits, rep.KeyframeTotal)
	fmt.Printf("keyframe_hit_rate=%.3f\n", keyframeRate)
	fmt.Printf("drift_ticks_per_frame=%s\n", rep.DriftRate)
	fmt.Printf("max_anchor_frame_error=%d\n", rep.MaxAnchorError)
	fmt.Printf("damage_spans=%d\n", rep.DamageSpans)
	fmt.Printf("recovered_frames=%d\n", rep.RecoveredFrames)
	fmt.Printf("unknown_chunks_marked=%d\n", rep.UnknownChunks)
	fmt.Printf("sha256_segments=%d\n", rep.SegmentTotal)
	fmt.Printf("sha256_match=%d/%d\n", rep.SegmentMatched, rep.SegmentTotal)
	fmt.Printf("output_sha256=%s\n", rep.FileSHA256)
	fmt.Printf("rerun_sha256=%s\n", rep.RerunSHA256)
	fmt.Printf("exit_code=%d\n", rep.ExitCode)
	return rep.ExitCode
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage: archivectl <command> [flags]")
	fmt.Fprintln(w, "commands:")
	fmt.Fprintln(w, "  demo [--seed N] [--frames N] [--channels N] [--inject list] [--workdir DIR]")
	fmt.Fprintln(w, "  scan --wav F [--clock F] [--checks F] [--index-out F]")
	fmt.Fprintln(w, "  index --wav F [--clock F] [--checks F] --out F")
	fmt.Fprintln(w, "  repair --index F [--wav F] [--clock F] --out F")
	fmt.Fprintln(w, "  verify --repaired F --baseline F")
}
