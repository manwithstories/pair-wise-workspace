// Command medpipe is the terminal adapter for the mobile voice-guidance
// media pipeline. It scans local MP4/MOV files, detects silence, deduplicates
// normalized windows and emits aligned segments with resumable checkpoints.
//
// Usage:
//
//	medpipe selftest --dir <dir> --seed <int> [--resume]
//
// Exit codes:
//
//	0 - breakpoints, corruption and checksum assertions passed
//	2 - invalid arguments
//	3 - unrecoverable media corruption
//	4 - input/output or checkpoint inconsistency
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"medpipe/internal/clock"
	"medpipe/internal/selftest"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return clock.KindArgument.ExitCode()
	}
	switch args[0] {
	case "selftest":
		return runSelfTest(args[1:])
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		return clock.KindArgument.ExitCode()
	}
}

func runSelfTest(args []string) int {
	opts := selftest.Options{Resume: false}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--dir":
			i++
			if i >= len(args) {
				return failArg("--dir requires a value")
			}
			opts.Dir = args[i]
		case arg == "--seed":
			i++
			if i >= len(args) {
				return failArg("--seed requires a value")
			}
			seed, err := strconv.Atoi(args[i])
			if err != nil {
				return failArg("--seed must be an integer: " + err.Error())
			}
			opts.Seed = int64(seed)
		case arg == "--resume":
			opts.Resume = true
		default:
			return failArg("unknown selftest flag " + arg)
		}
	}
	if opts.Dir == "" {
		return failArg("--dir is required")
	}
	if err := selftest.Run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "selftest failed:", err)
		return clock.ExitCode(err)
	}
	return 0
}

func failArg(msg string) int {
	fmt.Fprintln(os.Stderr, "argument error:", msg)
	usage()
	return clock.KindArgument.ExitCode()
}

func usage() {
	fmt.Fprintf(os.Stderr, strings.TrimSpace(`
medpipe - local MP4/MOV segment pipeline

usage:
  medpipe selftest --dir <dir> --seed <int> [--resume]

exit codes: 0 ok, 2 bad arguments, 3 media corruption, 4 inconsistency
`)+"\n")
}
