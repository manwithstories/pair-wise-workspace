// relaycheck is a terminal-only relay checker for regional live
// distribution: it demuxes CMAF/fMP4 segments, repairs timelines, remuxes
// losslessly, checkpoints progress and verifies provenance.
//
// Exit codes: 0 = completed and verified, 3 = resumable interruption,
// 4 = unrecoverable input or verification failure, 2 = usage error.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"relaycheck/internal/fixture"
	"relaycheck/internal/pipeline"
)

const usageText = `usage:
  relaycheck gen    --out DIR --seed N --segments N --faults gap,rollback,duplicate,truncate,loss,map-conflict
  relaycheck run    --input DIR --output DIR --index FILE [--chunk N] [--workers N] [--max-mem-mb N] [--stop-after N] [--resume]
  relaycheck verify --input DIR --output DIR --index FILE
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return 2
	}
	var err error
	switch args[0] {
	case "gen":
		err = cmdGen(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	default:
		fmt.Fprint(os.Stderr, usageText)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 4
	}
	return 0
}

func cmdGen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	seed := fs.Int64("seed", 1, "deterministic seed")
	segments := fs.Int("segments", 256, "number of segments")
	faults := fs.String("faults", "", "comma-separated fault types")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("--out is required")
	}
	var kinds []string
	if *faults != "" {
		kinds = strings.Split(*faults, ",")
	}
	m, err := fixture.Generate(fixture.Options{Out: *out, Seed: *seed, Segments: *segments, Faults: kinds})
	if err != nil {
		return err
	}
	fmt.Printf("generated segments=%d faults=%d out=%s\n", m.Segments, len(m.Faults), *out)
	for _, f := range m.Faults {
		fmt.Printf("fault type=%s seq=%d\n", f.Type, f.Seq)
	}
	return nil
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var cfg pipeline.Config
	fs.StringVar(&cfg.Input, "input", "", "input segment directory")
	fs.StringVar(&cfg.Output, "output", "", "output directory")
	fs.StringVar(&cfg.IndexPath, "index", "", "chunk index file")
	fs.IntVar(&cfg.Chunk, "chunk", 32, "segments per checkpoint chunk")
	fs.IntVar(&cfg.Workers, "workers", 4, "concurrent parse workers")
	fs.IntVar(&cfg.MaxMemMB, "max-mem-mb", 64, "in-flight buffer limit (MiB)")
	fs.IntVar(&cfg.StopAfter, "stop-after", 0, "stop after N committed chunks (resumable)")
	resume := fs.Bool("resume", false, "resume from the chunk index")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_ = *resume // resumability is inherent: committed chunks are always skipped
	if cfg.Input == "" || cfg.Output == "" || cfg.IndexPath == "" {
		fmt.Fprintln(os.Stderr, "run requires --input, --output and --index")
		return 2
	}
	rep, err := pipeline.Run(cfg)
	if errors.Is(err, pipeline.ErrInterrupted) {
		fmt.Fprintf(os.Stderr, "interrupted: committed chunks are checkpointed; rerun with --resume\n")
		return 3
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 4
	}
	rep.Print(os.Stdout)
	return 0
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	var cfg pipeline.VerifyConfig
	fs.StringVar(&cfg.Input, "input", "", "input segment directory")
	fs.StringVar(&cfg.Output, "output", "", "output directory")
	fs.StringVar(&cfg.IndexPath, "index", "", "chunk index file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cfg.Input == "" || cfg.Output == "" || cfg.IndexPath == "" {
		fmt.Fprintln(os.Stderr, "verify requires --input, --output and --index")
		return 2
	}
	rep, err := pipeline.Verify(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify failed: %v\n", err)
		return 4
	}
	rep.Print(os.Stdout)
	return 0
}
