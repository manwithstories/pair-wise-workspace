// Command mediad is the terminal adapter for deterministic TS alignment,
// damage localization, CMAF packaging and resumable HLS publication.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return 1
	}
	switch args[0] {
	case "demo":
		return runDemoCommand(args[1:])
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		printUsage(os.Stderr)
		return 1
	}
}

func runDemoCommand(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	opts := demoOptions{}
	fs.Int64Var(&opts.seed, "seed", 0, "deterministic fixture seed")
	fs.StringVar(&opts.dir, "dir", "", "output directory")
	fs.StringVar(&opts.faults, "faults", "all", "fault set to generate and verify: all|none")
	fs.IntVar(&opts.interruptAfter, "interrupt-after", 128, "packet index at which to simulate an interrupt")
	fs.BoolVar(&opts.resume, "resume", false, "verify hashes and resume after the simulated interrupt")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 1
	}
	if fs.NArg() != 0 {
		return usageError("unexpected arguments: %v", fs.Args())
	}
	return runDemo(opts)
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, "mediad - local H.264/AAC TS alignment and resumable CMAF/HLS demo")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  mediad demo --seed 73021 --dir DIR --faults all --interrupt-after 128 --resume")
}
