// Command fieldpost runs the local FieldPost media handoff demo and utilities.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"fieldpost/internal/core"
	"fieldpost/internal/demo"
)

func main() {
	os.Exit(runCLI(os.Args[1:]))
}

func runCLI(args []string) int {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return core.ExitUsage
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return core.ExitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		printUsage(os.Stderr)
		return core.ExitUsage
	}
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("fieldpost demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	seed := fs.Int64("seed", 42, "deterministic fixture seed")
	profile := fs.String("profile", "all", "demo profile (currently all)")
	memoryCap := fs.Int64("memory-cap", 8<<20, "maximum in-flight buffer bytes")
	outputDir := fs.String("output-dir", "", "output directory (default: temporary directory)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return core.ExitOK
		}
		return core.ExitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %v\n", fs.Args())
		return core.ExitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	report, err := demo.Run(ctx, demo.Options{
		Seed: *seed, Profile: *profile, MemoryCap: *memoryCap, OutputDir: *outputDir,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fieldpost: %v\n", err)
		return core.ExitCode(err)
	}
	fmt.Print(report.Output())
	return core.ExitOK
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, "usage: fieldpost demo --seed N --profile all --memory-cap BYTES [--output-dir DIR]")
	fmt.Fprintln(w, "exit codes: 0 success/expected rejections, 2 usage/manifest, 3 unrecoverable input, 4 invariant failure")
}
