// mediaslice is the acceptance entry point for the low-latency segment
// pipeline. It is a pure terminal tool: all behavior is driven by flags and
// all results are printed as stable key=value lines.
package main

import (
	"fmt"
	"os"
)

// Exit codes follow the acceptance contract:
//
//	0 - completed and validated
//	1 - parameter, I/O or internal error
//	2 - corrupt input or failed resume validation
const (
	exitOK       = 0
	exitInternal = 1
	exitCorrupt  = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitInternal
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "run":
		return runPipeline(args[1:])
	case "-h", "--help", "help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		usage()
		return exitInternal
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  mediaslice demo --seed N --duration 1800s --bitrates 800k,1600k,3200k \
                  --segment 2s --inject a,b --checkpoint-every 20s --resume
  mediaslice run --manifest sources.json --work-dir out --segment 2s

exit codes: 0 ok | 1 parameter/IO/internal | 2 corrupt input or resume failure`)
}
