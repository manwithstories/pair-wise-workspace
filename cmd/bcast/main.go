// Command bcast runs the local batch ingest, resume and evidence verification flow.
package main

import (
	"errors"
	"fmt"
	"os"

	"bcast/internal/fault"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, errHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "bcast:", err)
		os.Exit(fault.ExitCode(err))
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return fault.E(fault.InvalidArgument, "cli", "run", errors.New("missing command"))
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "demo":
		return runDemo(rest)
	case "resume":
		return runResume(rest)
	case "verify":
		return runVerify(rest)
	case "-h", "--help", "help":
		usage(os.Stdout)
		return errHelp
	default:
		usage(os.Stderr)
		return fault.E(fault.InvalidArgument, "cli", "run", fmt.Errorf("unknown command %q", cmd))
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage: bcast <demo|resume|verify> [flags]")
	fmt.Fprintln(w, "  demo   --root DIR [--seed N] [--selfcheck] [--interrupt-after N]")
	fmt.Fprintln(w, "  resume --root DIR")
	fmt.Fprintln(w, "  verify --root DIR [--full-scan]")
}
