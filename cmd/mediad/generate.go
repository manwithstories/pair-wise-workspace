package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"mediad/internal/fault"
	"mediad/internal/sample"
)

func runGenerate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("generate-fixture", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Int64("seed", 29, "deterministic seed")
	seconds := fs.Int("seconds", 12, "duration in seconds")
	out := fs.String("out", "", "fixture output directory")
	rate := fs.Int("rate", 44100, "PCM/RTP rate")
	channels := fs.Int("channels", 6, "L16 channels")
	clean := fs.Bool("clean", false, "do not inject faults")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return fault.ExitOK
		}
		return fault.ExitUsage
	}
	if *out == "" {
		fmt.Fprintln(stderr, "generate-fixture: --out is required")
		return fault.ExitUsage
	}
	m, err := sample.Generate(sample.Options{
		Seed: *seed, Seconds: *seconds, Rate: *rate, Channels: *channels,
		Dir: *out, Faults: !*clean,
	})
	if err != nil {
		fmt.Fprintf(stderr, "generate-fixture failed: %v\n", err)
		return fault.ExitCode(err)
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	fmt.Fprintln(stdout, string(b))
	return fault.ExitOK
}
