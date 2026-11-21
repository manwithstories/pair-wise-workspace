// Command archive is the terminal adapter for the RTP/RTCP restore pipeline.
// It generates a deterministic degraded capture, restores it into verifiable
// shards and audits the result without any network access.
package main

import (
	"flag"
	"fmt"
	"os"
)

// Exit codes from the archive contract.
const (
	exitOK               = 0
	exitBadArgs          = 1
	exitDataCorruption   = 2
	exitUnsafeCheckpoint = 3
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitBadArgs)
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		// Subcommands already printed their own summary including exit_code;
		// main only needs to terminate with the contracted status.
		os.Exit(mapError(err))
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `archive - local RTP/RTCP capture restoration

Usage:
  archive demo   --seed N --loss P --reorder P --wrap-at N --drift-ppm N
                 [--vfr] [--resume] --checkpoint-every N --out DIR
  archive verify --out DIR [--inject torn-tail] [--compare DIR2]

Exit codes: 0 ok | 1 bad arguments/input | 2 locatable data corruption
             | 3 checkpoint or index cannot be safely resumed
`)
}

// demoFlags holds the parsed demo parameters.
type demoFlags struct {
	seed            int64
	loss            int
	reorder         int
	duplicate       int
	wrapAt          uint64
	driftPPM        int64
	vfr             bool
	resume          bool
	checkpointEvery int
	videoPackets    int
	audioPackets    int
	out             string
}

func parseDemo(args []string) (demoFlags, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	var d demoFlags
	fs.Int64Var(&d.seed, "seed", 41, "deterministic generator seed")
	fs.IntVar(&d.loss, "loss", 3, "loss percentage 0..100")
	fs.IntVar(&d.reorder, "reorder", 7, "reorder percentage 0..100")
	fs.IntVar(&d.duplicate, "duplicate", 4, "duplicate percentage 0..100")
	fs.Uint64Var(&d.wrapAt, "wrap-at", 0, "RTP timestamp wrap modulus (0 = 2^32)")
	fs.Int64Var(&d.driftPPM, "drift-ppm", 0, "device clock drift in ppm")
	fs.BoolVar(&d.vfr, "vfr", false, "enable variable frame rate")
	fs.BoolVar(&d.resume, "resume", false, "resume from an existing checkpoint")
	fs.IntVar(&d.checkpointEvery, "checkpoint-every", 256, "ticks per checkpoint/shard")
	fs.IntVar(&d.videoPackets, "video-packets", 1200, "generated video RTP packets")
	fs.IntVar(&d.audioPackets, "audio-packets", 600, "generated audio RTP packets")
	fs.StringVar(&d.out, "out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return d, err
	}
	if d.out == "" {
		return d, fmt.Errorf("--out is required")
	}
	if d.checkpointEvery <= 0 {
		return d, fmt.Errorf("--checkpoint-every must be positive")
	}
	return d, nil
}

// verifyFlags holds the parsed verify parameters.
type verifyFlags struct {
	out     string
	inject  string
	compare string
}

func parseVerify(args []string) (verifyFlags, error) {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	var v verifyFlags
	fs.StringVar(&v.out, "out", "", "restored output directory")
	fs.StringVar(&v.inject, "inject", "", "fault to inject: torn-tail")
	fs.StringVar(&v.compare, "compare", "", "second output dir for cross-run hash comparison")
	if err := fs.Parse(args); err != nil {
		return v, err
	}
	if v.out == "" {
		return v, fmt.Errorf("--out is required")
	}
	if v.inject != "" && v.inject != "torn-tail" {
		return v, fmt.Errorf("unsupported --inject %q (only torn-tail)", v.inject)
	}
	return v, nil
}
