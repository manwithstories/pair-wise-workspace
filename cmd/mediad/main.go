// Command mediad is the acceptance entry point: it reassembles multi-track
// RTP audio onto a unified timeline, detects silence, fingerprints
// segments, round-trips MP4 and recovers from checkpoints.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mediad <verify> [flags]")
		os.Exit(1)
	}
	switch os.Args[1] {
	case "verify":
		os.Exit(cmdVerify(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(1)
	}
}
