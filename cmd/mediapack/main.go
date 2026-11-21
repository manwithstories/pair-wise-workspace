// Command mediapack is the terminal entry point for the media packaging
// pipeline.  It has two forms:
//
//	mediapack demo [flags]   run the end-to-end demonstration
//	mediapack help           print usage
//
// Exit codes are part of the contract: 0 success, 1 bad command line, 2 a
// detected media or dependency failure.
package main

import (
	"fmt"
	"io"
	"os"

	"mediapack/internal/fault"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 1
	}
	switch args[0] {
	case "demo":
		opts, err := parseDemo(args[1:], stderr)
		if err != nil {
			fmt.Fprintf(stderr, "mediapack: %v\n", err)
			return 1
		}
		result := demo(opts, stdout)
		return result.ExitCode
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		usage(stderr)
		return 1
	}
}

// reportFailure prints the machine-readable failure lines shared by every
// blocking path: the error itself, the blocked status, and the first blocked
// unit (a frame when known, otherwise the named dependency).  The first
// blocked unit is the one operators should regenerate, so it is chosen from
// the attributable error rather than from whichever stage happened to print
// last.
func reportFailure(w io.Writer, err error) {
	fmt.Fprintf(w, "error=%s\n", err)
	fmt.Fprintf(w, "status=blocked\n")
	fmt.Fprintf(w, "first_blocked=%s\n", firstBlockedName(err))
}

// firstBlockedName renders the single earliest blocked unit.
func firstBlockedName(err error) string {
	fe, ok := fault.As(err)
	if !ok {
		return "unknown"
	}
	switch {
	case fe.Frame > 0:
		return fmt.Sprintf("frame-%d", fe.Frame)
	case fe.Segment > 0:
		return fmt.Sprintf("segment-%d", fe.Segment)
	case fe.BlockedBy != "":
		return fe.BlockedBy
	case fe.Track != "" && fe.Track != "*":
		return fe.Track
	default:
		return fe.Stage
	}
}
