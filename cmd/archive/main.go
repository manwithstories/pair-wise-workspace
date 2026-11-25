// Command archive is the terminal front end for the media archive remux
// pipeline. It never decodes or re-encodes media; it validates, re-times,
// re-indexes and re-packages MP4/MOV containers losslessly.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"mediaarchive/cmd/archive/internal/runner"
	"mediaarchive/internal/fixture"
	"mediaarchive/internal/manifest"
	"mediaarchive/internal/model"
)

// Exit codes are part of the archive contract.
const (
	exitOK             = 0
	exitUsage          = 2
	exitUnrecoverable  = 3
	exitCanceled       = 4
	exitVerifyMismatch = 5
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
	case "fixture":
		return cmdFixture(args[1:])
	case "remux":
		return cmdRemux(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		return exitUsage
	}
}

func cmdFixture(args []string) int {
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	dir := fs.String("dir", "", "output directory for generated fixtures")
	cases := fs.String("case", "all", "case name or 'all'")
	seed := fs.Int64("seed", 17, "deterministic RNG seed")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "fixture: --dir is required")
		return exitUsage
	}
	paths, err := fixture.GenerateAll(*dir, splitCSV(*cases), *seed)
	if err != nil {
		return classify(err)
	}
	for _, p := range paths {
		fmt.Println("generated", p)
	}
	return exitOK
}

func cmdRemux(args []string) int {
	fs := flag.NewFlagSet("remux", flag.ContinueOnError)
	o := runner.Options{}
	fs.StringVar(&o.Input, "input", "", "input MP4/MOV path")
	fs.StringVar(&o.Output, "output", "", "output MP4 path")
	fs.StringVar(&o.Manifest, "manifest", "", "manifest JSON path")
	fs.StringVar(&o.Checkpoint, "checkpoint", "", "checkpoint path")
	fs.StringVar(&o.MapFile, "map", "", "explicit track mapping JSON")
	fs.IntVar(&o.Workers, "workers", 4, "bounded worker count")
	fs.IntVar(&o.Queue, "queue", 8, "bounded job queue length")
	mem := fs.Int64("max-buffer-bytes", 256<<20, "buffered payload memory budget")
	fs.Int64Var(&o.TargetBPS, "target-bps", 0, "target bit rate in bps")
	fs.StringVar(&o.OnDamage, "on-damage", "fail", "fail|recover")
	fs.IntVar(&o.StopAfter, "stop-after", 0, "cancel after N samples (checkpointed)")
	fs.BoolVar(&o.Resume, "resume", false, "resume from checkpoint")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	o.BufferMem = *mem
	if missing := validateRemux(o); missing != "" {
		fmt.Fprintln(os.Stderr, "remux:", missing)
		return exitUsage
	}
	if o.OnDamage != "fail" && o.OnDamage != "recover" {
		fmt.Fprintln(os.Stderr, "remux: --on-damage must be fail or recover")
		return exitUsage
	}
	out, err := runner.Run(context.Background(), o)
	if errors.Is(err, runner.ErrCanceled) {
		fmt.Printf("canceled after %d samples; checkpoint=%s\n", o.StopAfter, o.Checkpoint)
		return exitCanceled
	}
	if err != nil {
		return classify(err)
	}
	stats, ordered, durationNS, err := runner.Finalize(out, o)
	if err != nil {
		return classify(err)
	}
	q := runner.BuildQuality(out, stats, ordered, durationNS, o.TargetBPS)
	mf, err := manifest.Build(o.Input, o.Output, out.InputSHA, q)
	if err != nil {
		return classify(err)
	}
	if err := mf.Save(o.Manifest); err != nil {
		return classify(err)
	}
	printSummary(mf)
	return exitOK
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	input := fs.String("input", "", "source MP4/MOV path")
	output := fs.String("output", "", "remuxed MP4 path")
	mfPath := fs.String("manifest", "", "manifest JSON path")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *input == "" || *output == "" || *mfPath == "" {
		fmt.Fprintln(os.Stderr, "verify: --input, --output and --manifest are required")
		return exitUsage
	}
	res, err := runner.Verify(*input, *output, *mfPath)
	if res != nil {
		fmt.Printf("verify: compared=%d mismatches=%d match=%.2f%%\n",
			res.Compared, res.Mismatches, res.MatchPct)
	}
	if err != nil {
		return classify(err)
	}
	fmt.Println("verify: lossless packet identity confirmed")
	return exitOK
}

func validateRemux(o runner.Options) string {
	if o.Input == "" || o.Output == "" || o.Manifest == "" {
		return "--input, --output and --manifest are required"
	}
	if o.Workers <= 0 || o.Queue <= 0 || o.BufferMem <= 0 {
		return "--workers, --queue and --max-buffer-bytes must be positive"
	}
	return ""
}

// classify maps the unified error model to the archive exit code contract.
func classify(err error) int {
	var me *model.Error
	if errors.As(err, &me) {
		if me.Recoverable {
			// A recoverable error that reached the top level means no damage
			// policy handled it: treat as unrecoverable corruption.
			return exitUnrecoverable
		}
		switch me.Stage {
		case model.StageManifest:
			return exitVerifyMismatch
		case model.StageCLI, model.StageTrack:
			return exitUsage
		case model.StageDemux:
			// A file that cannot be opened at all is an input error (2);
			// structural corruption of an opened file is unrecoverable (3).
			if strings.Contains(me.Reason, "cannot open") ||
				strings.Contains(me.Reason, "cannot stat") {
				return exitUsage
			}
			return exitUnrecoverable
		default:
			return exitUnrecoverable
		}
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	return exitUnrecoverable
}

func printSummary(mf *manifest.Manifest) {
	fmt.Printf("target_bps=%d\n", mf.TargetBPS)
	fmt.Printf("actual_bps=%d\n", mf.ActualBPS)
	fmt.Printf("deviation_pct=%.2f\n", mf.DeviationPct)
	fmt.Printf("sample_sha256_match=%.0f%%\n", mf.SampleSHA256Match)
	fmt.Printf("dropped_samples=%d\n", mf.DroppedSamples)
	fmt.Printf("manifest_max_error_ns=%d\n", mf.MaxErrorNS)
	fmt.Printf("recovered=%v\n", mf.Recovered)
}

func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func usage() {
	fmt.Fprintln(os.Stderr,
		"usage: archive <fixture|remux|verify> [flags]\n"+
			"  fixture  --dir DIR --case all --seed 17\n"+
			"  remux    --input IN --output OUT --manifest M [flags]\n"+
			"  verify   --input IN --output OUT --manifest M")
}
