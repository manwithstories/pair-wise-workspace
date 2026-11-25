// Command relaycheck connects demo flags to the media kernel and reports the
// deterministic acceptance summary. All processing is local.
package main

import (
	"flag"
	"fmt"
	"os"

	"relaycheck/internal/pipeline"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: relaycheck demo [flags]")
		os.Exit(1)
	}

	def := pipeline.DefaultConfig()
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", def.Seed, "deterministic seed")
	targetRate := fs.Int("target-rate", def.TargetRate, "target sample rate")
	layout := fs.String("layout", def.Layout, "output channel layout (stereo)")
	lufs := fs.Float64("integrated-lufs", def.IntegratedLUFS, "target integrated loudness")
	dupEvery := fs.Int("duplicate-every", def.DuplicateEvery, "duplicate every Nth packet")
	window := fs.Int("reorder-window", def.ReorderWindow, "RTP reorder window")
	burstLoss := fs.Int("burst-loss", def.BurstLoss, "consecutive packets lost in one burst")
	bandwidth := fs.String("bandwidth-kbps", "12000,4000,9000", "comma-separated bandwidth steps kbps")
	out := fs.String("out", def.Out, "output directory")
	caseName := fs.String("case", "", "named fixture case (e.g. truncated-mp4)")
	crashAfter := fs.Int("crash-after-segment", -1, "inject interrupt after segment N prepare")
	resume := fs.Bool("resume", false, "recover WAL and continue")

	if err := fs.Parse(os.Args[2:]); err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		os.Exit(2)
	}

	steps, bf := pipeline.ParseBandwidth(*bandwidth)
	if bf != nil {
		fmt.Print(pipeline.FormatFault(bf))
		os.Exit(bf.ExitCode())
	}

	cfg := pipeline.Config{
		Seed: *seed, TargetRate: *targetRate, Layout: *layout,
		IntegratedLUFS: *lufs, DuplicateEvery: *dupEvery,
		ReorderWindow: *window, BurstLoss: *burstLoss, BandwidthKbps: steps,
		Out: *out, Case: *caseName, CrashAfterSegment: *crashAfter, Resume: *resume,
	}

	res, fault := pipeline.Run(cfg)
	if fault != nil {
		fmt.Print(pipeline.FormatFault(fault))
		fmt.Fprintln(os.Stderr, fault.Error())
		os.Exit(fault.ExitCode())
	}

	fmt.Print(pipeline.FormatResult(res))
	fmt.Fprint(os.Stderr, pipeline.FormatDiagnostics(res))
	os.Exit(0)
}
