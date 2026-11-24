// Command mediasim is the acceptance and demo entry point. It never opens a
// window, browser or capture device: everything runs locally in a terminal.
package main

import (
	"flag"
	"fmt"
	"os"

	"mediasim/internal/clock"
	"mediasim/internal/pipeline"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return pipeline.ExitError
	}
	switch args[0] {
	case "demo":
		return demos(args[1:], "demo")
	case "verify":
		return verify(args[1:])
	case "fixtures":
		return listFixtures(args[1:])
	case "-h", "--help", "help":
		usage()
		return pipeline.ExitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage()
		return pipeline.ExitError
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mediasim - local tactical replay media simulator

usage:
  mediasim demo   [--fixture NAME] [--seed N] [--loss-burst N] [--jitter-ms N] [--out DIR]
  mediasim verify --fixture truncated --out DIR
  mediasim fixtures

exit codes: 0 complete, 1 error, 2 partial recovery (report still written)
`)
}

func demos(args []string, _ string) int {
	cfg := pipeline.Config{}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.StringVar(&cfg.Fixture, "fixture", "normal", "fixture: normal, truncated, corrupt, missing-index")
	fs.Int64Var(&cfg.Seed, "seed", 17, "deterministic seed")
	fs.IntVar(&cfg.LossBurst, "loss-burst", 0, "packets dropped per burst window")
	fs.IntVar(&cfg.JitterMs, "jitter-ms", 0, "jitter buffer target delay in ms")
	fs.Int64Var(&cfg.JitterMax, "jitter-max-ms", 0, "jitter amplitude in ms")
	fs.StringVar(&cfg.OutDir, "out", "./artifacts", "output directory")
	fs.IntVar(&cfg.Window, "window", 16, "RTP reorder window in packets")
	if err := fs.Parse(args); err != nil {
		return pipeline.ExitError
	}
	return execute(cfg)
}

func verify(args []string) int {
	cfg := pipeline.Config{}
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.StringVar(&cfg.Fixture, "fixture", "truncated", "fixture to verify")
	fs.Int64Var(&cfg.Seed, "seed", 17, "deterministic seed")
	fs.StringVar(&cfg.OutDir, "out", "./artifacts-bad", "output directory")
	fs.IntVar(&cfg.LossBurst, "loss-burst", 4, "packets dropped per burst window")
	fs.IntVar(&cfg.JitterMs, "jitter-ms", 80, "jitter buffer target delay in ms")
	fs.Int64Var(&cfg.JitterMax, "jitter-max-ms", 25, "jitter amplitude in ms")
	if err := fs.Parse(args); err != nil {
		return pipeline.ExitError
	}
	return execute(cfg)
}

func listFixtures(args []string) int {
	fs := flag.NewFlagSet("fixtures", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return pipeline.ExitError
	}
	for _, name := range []string{"normal", "truncated", "corrupt", "missing-index"} {
		fmt.Println(name)
	}
	return pipeline.ExitOK
}

// execute runs the pipeline, prints the contract fields and the report path,
// and maps every failure onto exit 1 with a classified message on stderr.
func execute(cfg pipeline.Config) int {
	res, err := pipeline.Run(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s: %v\n", clock.Classify(err), err)
		return pipeline.ExitError
	}
	reportPath, werr := pipeline.WriteReport(cfg.OutDir, res.Report)
	res.Report.Fields(os.Stdout)
	if werr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", werr)
		return pipeline.ExitError
	}
	fmt.Printf("report=%s\n", reportPath)
	fmt.Printf("summary: %s\n", res.Report.String())
	if res.Report.ExitCode == pipeline.ExitPartial {
		fmt.Fprintln(os.Stderr, "partial recovery: report written, some frames or ranges were not recoverable")
	}
	return res.Report.ExitCode
}
