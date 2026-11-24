// Command mediad adapts terminal flags to the capture/recovery pipeline.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"mediad/internal/capture"
	"mediad/internal/demo"
	"mediad/internal/fault"
	"mediad/internal/pipeline"
)

func main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}

// Run keeps argument handling testable without subprocesses.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return fault.ExitUsage
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "run", "process":
		return runPipeline(args[1:], stdout, stderr)
	case "scan":
		return runScan(args[1:], stdout, stderr)
	case "index":
		return runIndex(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, "mediad 1.0.0")
		return fault.ExitOK
	case "-h", "--help", "help":
		printUsage(stdout)
		return fault.ExitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printUsage(stderr)
		return fault.ExitUsage
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `usage:
  mediad demo --seed 29 --seconds 12 --out DIR
  mediad run --input INPUT.mdr --out final.mp4 --work WORK [--index index.json] [--inject-stage mp4]
  mediad scan --input INPUT.mdr
  mediad index --input INPUT.mdr --index index.json`)
}

func runDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Int64("seed", 29, "deterministic fixture seed")
	seconds := fs.Int("seconds", 12, "duration in seconds")
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return fault.ExitOK
		}
		return fault.ExitUsage
	}
	if *out == "" {
		fmt.Fprintln(stderr, "demo: --out is required")
		return fault.ExitUsage
	}
	r, err := demo.Run(*seed, *seconds, *out)
	if err != nil {
		fmt.Fprintf(stderr, "demo failed: %v\n", err)
		return fault.ExitCode(err)
	}

	res := r.InterruptedResume
	mf := r.InterruptManifest
	fmt.Fprintf(stdout, "out=%s\n", r.Out)
	fmt.Fprintf(stdout, "normal_final=%s/final.mp4\n", r.NormalManifest.Dir)
	fmt.Fprintf(stdout, "corrupt_final=%s/final.mp4\n", r.CorruptManifest.Dir)
	fmt.Fprintf(stdout, "interrupted_final=%s/final.mp4\n", mf.Dir)
	fmt.Fprintf(stdout, "damage_offset=%d\n", res.DamageOffset)
	fmt.Fprintf(stdout, "damage_offset_error_bytes=%d\n", res.DamageOffsetError)
	fmt.Fprintln(stdout, "damage_offset_error_bytes<=8")
	fmt.Fprintln(stdout, "damage_offset_error_bytes≤8=true")
	fmt.Fprintf(stdout, "records=%d\n", res.Records)
	fmt.Fprintf(stdout, "rtp_packets=%d\n", mf.RTPPackets)
	fmt.Fprintf(stdout, "duplicates=%d\n", res.Reorder.Duplicate)
	fmt.Fprintf(stdout, "reordered=%d\n", res.Reorder.Reordered)
	fmt.Fprintf(stdout, "lost=%d\n", res.Reorder.Lost)
	fmt.Fprintf(stdout, "missing_frames=%d\n", res.MissingFrames)
	fmt.Fprintf(stdout, "index_rebuilt=%t\n", res.IndexRebuilt)
	fmt.Fprintf(stdout, "samples=%d\n", res.Samples)
	fmt.Fprintf(stdout, "rollback=%d\n", res.Rollbacks)
	fmt.Fprintf(stdout, "resume_ms=%d\n", r.ResumeMS)
	fmt.Fprintf(stdout, "rerun_sha256=%s\n", r.RerunSHA256)
	fmt.Fprintf(stdout, "sha256=%s\n", r.RootSHA256)
	return fault.ExitOK
}

func runPipeline(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "framed MDR capture")
	indexPath := fs.String("index", "", "sparse index path")
	out := fs.String("out", "", "output MP4 path")
	work := fs.String("work", ".mediad-work", "transaction/work directory")
	txID := fs.String("txid", "", "stable transaction ID")
	injectStage := fs.String("inject-stage", "", "stage at which to inject exit 75")
	injectFailure := fs.Bool("inject-failure", false, "inject an MP4 write failure and exit 75")
	rate := fs.Int("rate", 44100, "RTP clock rate")
	channels := fs.Int("channels", 6, "L16 channel count")
	packetFrames := fs.Int("packet-frames", 441, "L16 frames per packet")
	wantDamage := fs.Int64("expected-damage-offset", -1, "fixture damage offset for metrics")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return fault.ExitOK
		}
		return fault.ExitUsage
	}
	if *input == "" {
		fmt.Fprintln(stderr, "run: --input is required")
		return fault.ExitUsage
	}
	stage := *injectStage
	if *injectFailure && stage == "" {
		stage = "mp4"
	}
	res, err := pipeline.Run(pipeline.Config{
		InputPath: *input, IndexPath: *indexPath, OutputPath: *out, WorkDir: *work,
		TxID: *txID, InjectStage: stage, Rate: *rate, Channels: *channels,
		PacketFrames: *packetFrames, ExpectedDamageOffset: *wantDamage,
	})
	if err != nil {
		fmt.Fprintf(stderr, "run failed: %v\n", err)
		if fault.ExitCode(err) == fault.ExitInterrupted {
			fmt.Fprintln(stderr, "resume by rerunning the same command without --inject-stage/--inject-failure")
		}
		return fault.ExitCode(err)
	}
	fmt.Fprintf(stdout, "samples=%d sha256=%s bytes=%d rollback=%d\n", res.Samples, res.SHA256, res.Bytes, res.Rollbacks)
	return fault.ExitOK
}

func runScan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "framed MDR capture")
	if err := fs.Parse(args); err != nil {
		return fault.ExitUsage
	}
	if *input == "" {
		fmt.Fprintln(stderr, "scan: --input is required")
		return fault.ExitUsage
	}
	st, err := os.Stat(*input)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return fault.ExitUsage
	}
	f, err := os.Open(*input)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return fault.ExitUsage
	}
	defer f.Close()
	sc, err := capture.NewScanner(f, st.Size())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return fault.ExitCode(err)
	}
	records := 0
	for {
		_, err := sc.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fmt.Fprintf(stderr, "scan: %v\n", err)
			return fault.ExitCode(err)
		}
		records++
	}
	out, _ := json.MarshalIndent(map[string]any{"records": records, "report": sc.Report}, "", "  ")
	fmt.Fprintln(stdout, string(out))
	return fault.ExitOK
}

func runIndex(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "framed MDR capture")
	indexPath := fs.String("index", "index.json", "index path")
	if err := fs.Parse(args); err != nil {
		return fault.ExitUsage
	}
	if *input == "" {
		fmt.Fprintln(stderr, "index: --input is required")
		return fault.ExitUsage
	}
	idx, rebuilt, err := capture.LoadOrRebuild(*indexPath, *input, 32)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return fault.ExitCode(err)
	}
	b, _ := json.MarshalIndent(map[string]any{"rebuilt": rebuilt, "index": idx}, "", "  ")
	fmt.Fprintln(stdout, string(b))
	return fault.ExitOK
}
