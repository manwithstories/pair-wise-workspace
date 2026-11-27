// Command avpipe runs the interactive audio pipeline demo.
//
// Exit codes: 0 demo and verification passed, 2 parameter or graph error,
// 3 strict-mode input corruption, 4 I/O, cancellation or internal failure.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"

	"avpipe/internal/demo"
	"avpipe/internal/media"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return int(media.ExitUsage)
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "version", "--version", "-v":
		fmt.Printf("avpipe %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
		return int(media.ExitOK)
	case "help", "--help", "-h":
		usage(os.Stdout)
		return int(media.ExitOK)
	default:
		fmt.Fprintf(os.Stderr, "avpipe: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return int(media.ExitUsage)
	}
}

const version = "0.1.0"

func runDemo(args []string) int {
	opts := demo.DefaultOptions()
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Int64Var(&opts.Seed, "seed", opts.Seed, "deterministic fixture seed")
	fs.IntVar(&opts.Queue, "queue", opts.Queue, "bounded queue capacity (blocks)")
	fs.IntVar(&opts.ConsumerStallMS, "consumer-stall-ms", opts.ConsumerStallMS, "per-block consumer stall in ms")
	fs.IntVar(&opts.CorruptTailBytes, "corrupt-tail-bytes", opts.CorruptTailBytes, "bytes stripped from the truncated fixture")
	fs.BoolVar(&opts.VerifyRepeat, "verify-repeat", opts.VerifyRepeat, "re-run and compare segment hashes")
	fs.StringVar(&opts.Out, "out", opts.Out, "output directory")
	fs.IntVar(&opts.Frames, "frames", opts.Frames, "program length in frames at 48 kHz")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: avpipe demo [flags]")
		fmt.Fprintln(os.Stderr, "  renders the generated fixture through resample, layout, loudness,")
		fmt.Fprintln(os.Stderr, "  bounded queue and audit, then prints the acceptance report.")
		fmt.Fprintln(os.Stderr, "flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return int(media.ExitUsage)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "avpipe demo: unexpected arguments %v\n", fs.Args())
		return int(media.ExitUsage)
	}

	rep, err := demo.Run(context.Background(), opts, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "avpipe demo: %v\n", err)
		return int(media.ExitFor(err))
	}
	if !rep.Passed() {
		return int(media.ExitInternal)
	}
	return int(media.ExitOK)
}

func usage(w *os.File) {
	fmt.Fprintln(w, "avpipe - interactive audio pipeline demo")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "usage: avpipe <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "commands:")
	fmt.Fprintln(w, "  demo     run the end-to-end pipeline and the acceptance checks")
	fmt.Fprintln(w, "  version  print the build version")
	fmt.Fprintln(w, "  help     print this message")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "exit codes: 0 pass, 2 usage/graph, 3 strict input corruption, 4 internal")
}
