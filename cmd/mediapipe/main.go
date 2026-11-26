package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(4)
	}
	switch os.Args[1] {
	case "demo":
		if err := runDemo(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "demo:", err)
			os.Exit(exitCode(err))
		}
	default:
		usage()
		os.Exit(4)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mediapipe demo --work DIR [--seed N] [--seconds N] [--target-kbps N] [--fault-suite]")
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	work := fs.String("work", ".tmp/demo", "working directory")
	seed := fs.Int64("seed", 1, "deterministic seed")
	seconds := fs.Int("seconds", 30, "generated media duration")
	targetKbps := fs.Int("target-kbps", 1200, "target total bitrate")
	faultSuite := fs.Bool("fault-suite", false, "run corruption and recovery fixtures")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Printf(`{"event":"skeleton","work":%q,"seed":%d,"seconds":%d,"target_kbps":%d,"fault_suite":%t}`+"\n",
		*work, *seed, *seconds, *targetKbps, *faultSuite)
	return nil
}

func exitCode(err error) int {
	type coder interface{ ExitCode() int }
	if c, ok := err.(coder); ok {
		return c.ExitCode()
	}
	return 1
}
