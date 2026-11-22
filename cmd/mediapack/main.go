// Command mediapack is the terminal adapter over the mediapack pipeline. It
// generates synthetic media in-process (no external files), runs the bounded
// resumable pipeline, verifies checksums and maps faults to exit codes:
//
//	0 success, 2 corrupt input, 3 checksum/source failure, 4 I/O/recovery.
package main

import (
	"fmt"
	"os"

	"mediapack/internal/fault"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 4
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		return 4
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mediapack - bounded resumable media fragment pipeline

usage:
  mediapack demo   --seed N --bytes N --chunk N --slow-ms N --interrupt-after N --resume
  mediapack verify --seed N
`)
}

type demoFlags struct {
	seed          int64
	bytes         int
	chunk         int
	slowMs        int
	interruptAt   int
	resume        bool
}

func parseFlags(args []string) (demoFlags, error) {
	var f demoFlags
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag %s needs a value", a)
			}
			i++
			return args[i], nil
		}
		var err error
		switch a {
		case "--seed":
			var v string
			if v, err = next(); err != nil {
				return f, err
			}
			_, err = fmt.Sscan(v, &f.seed)
		case "--bytes":
			var v string
			if v, err = next(); err != nil {
				return f, err
			}
			_, err = fmt.Sscan(v, &f.bytes)
		case "--chunk":
			var v string
			if v, err = next(); err != nil {
				return f, err
			}
			_, err = fmt.Sscan(v, &f.chunk)
		case "--slow-ms":
			var v string
			if v, err = next(); err != nil {
				return f, err
			}
			_, err = fmt.Sscan(v, &f.slowMs)
		case "--interrupt-after":
			var v string
			if v, err = next(); err != nil {
				return f, err
			}
			_, err = fmt.Sscan(v, &f.interruptAt)
		case "--resume":
			f.resume = true
		default:
			return f, fmt.Errorf("unknown flag %q", a)
		}
		if err != nil {
			return f, fmt.Errorf("bad value for %s: %w", a, err)
		}
	}
	return f, nil
}

// exitFor maps a structured fault onto the documented process exit codes.
func exitFor(err error) int {
	fe, ok := fault.As(err)
	if !ok {
		return 4
	}
	switch fe.Kind {
	case fault.KindCorruptInput:
		return 2
	case fault.KindChecksum:
		return 3
	default:
		return 4
	}
}
