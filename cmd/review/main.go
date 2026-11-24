// Command review runs the local, off-screen Matroska/WebM review demo.
//
// Usage:
//
//	review demo --seed <int> --out <absolute-dir>
//
// Exit codes: 0 all checks passed; 2 input boundary rejected; 3 internal
// or recovery failure.
package main

import (
	"flag"
	"fmt"
	"os"

	"review/internal/reviewapp"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(reviewapp.ExitBoundary)
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(reviewapp.ExitOK)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(reviewapp.ExitBoundary)
	}
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic fixture seed")
	outDir := fs.String("out", "", "absolute output directory (required)")
	if err := fs.Parse(args); err != nil {
		return reviewapp.ExitBoundary
	}
	if *outDir == "" {
		fmt.Fprintln(os.Stderr, "demo: --out is required and must be an absolute path")
		return reviewapp.ExitBoundary
	}
	rendered, code, err := reviewapp.Run(reviewapp.Config{Seed: *seed, Out: *outDir})
	if rendered != "" {
		fmt.Print(rendered)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
	}
	return code
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: review demo --seed <int> --out <absolute-dir>")
}
