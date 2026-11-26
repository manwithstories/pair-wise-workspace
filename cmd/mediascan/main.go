package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if len(args) == 0 {
		printUsage(os.Stderr)
		return 1
	}
	switch args[0] {
	case "demo":
		return runDemoCommand(ctx, args[1:])
	case "scan":
		return runScanCommand(ctx, args[1:])
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		printUsage(os.Stderr)
		return 1
	}
}

func runDemoCommand(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	work := fs.String("work", "./.demo", "demo workspace directory")
	workers := fs.Int("workers", 4, "worker concurrency (1-16)")
	seed := fs.Int64("seed", 17, "deterministic fixture seed")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *workers < 1 || *workers > 16 {
		fmt.Fprintf(os.Stderr, "workers must be between 1 and 16, got %d\n", *workers)
		return 1
	}
	_, code, err := runDemo(ctx, demoOptions{WorkDir: *work, Workers: *workers, Seed: *seed})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediascan demo failed: %v\n", err)
		if code == 4 {
			return 4
		}
		if errors.Is(err, context.Canceled) {
			return 3
		}
		return 1
	}
	return code
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, "usage: mediascan <demo|scan> [flags]")
	fmt.Fprintln(w, "  demo --work ./.demo --workers 4 --seed 17")
	fmt.Fprintln(w, "  scan --work ./.scan [path ...]")
}
