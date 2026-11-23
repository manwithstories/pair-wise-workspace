// Command archivecheck restores CMAF/fMP4 archive fragments: it parses an
// explicit input manifest, validates every box and sample range, repairs the
// decode timeline, builds a random-access index and proves the result with an
// independent re-read.
//
// Exit codes: 0 verified, 2 input or verification failure, 3 incompatible
// resume state, 4 processing cancelled.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"archivecheck/internal/fault"
	"archivecheck/internal/pipe"
)

const (
	exitOK           = 0
	exitFailure      = 2
	exitIncompatible = 3
	exitCancelled    = 4
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitFailure
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "resume":
		return runResume(args[1:])
	case "-h", "--help", "help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "archivecheck: unknown command %q\n", args[0])
		usage()
		return exitFailure
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `archivecheck — restore and verify archived CMAF/fMP4 fragments

Usage:
  archivecheck demo   --work DIR --queue N [--slow-consumer D] [--interrupt-after N] [--resume]
  archivecheck verify --work DIR
  archivecheck resume --work DIR [--queue N] [--slow-consumer D]

Exit codes: 0 verified, 2 input/verification failure, 3 incompatible resume
state, 4 processing cancelled.
`)
}

func fail(err error) int {
	if err == nil {
		return exitFailure
	}
	fmt.Fprintf(os.Stderr, "archivecheck: %v\n", err)
	if pipe.IsCancelled(err) {
		return exitCancelled
	}
	if fault.IsKind(err, fault.KindCheckpoint) {
		return exitIncompatible
	}
	return exitFailure
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
