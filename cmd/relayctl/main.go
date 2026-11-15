// Command relayctl runs and verifies a loopback-only one-to-many RTP relay.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"pairwise.local/relayctl/internal/guard"
	"pairwise.local/relayctl/internal/signal"
)

const (
	exitOK          = 0
	exitConfig      = 2
	exitNonLoopback = 3
	exitSignal      = 4
	exitThreshold   = 5
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitConfig
	}
	switch args[0] {
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "serve":
		return runServe(args[1:], stderr)
	case "publish":
		return runPublish(args[1:], stdout, stderr)
	case "subscribe":
		return runSubscribe(args[1:], stdout, stderr)
	case "report":
		return runReport(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		usage(stderr)
		return exitConfig
	}
}

func usage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: relayctl <serve|publish|subscribe|verify|report> [flags]")
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags
}

func parseFlags(flags *flag.FlagSet, args []string) bool {
	if err := flags.Parse(args); err != nil {
		return false
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(flags.Output(), "unexpected arguments: %v\n", flags.Args())
		return false
	}
	return true
}

func codeForError(err error) int {
	if err == nil {
		return exitOK
	}
	if errors.Is(err, guard.ErrNonLoopback) {
		return exitNonLoopback
	}
	var remote *signal.RemoteError
	if errors.As(err, &remote) && remote.Code == signal.CodeHandshakeTimeout {
		return exitSignal
	}
	return exitSignal
}
