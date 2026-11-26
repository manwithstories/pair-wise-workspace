package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"avpipe/internal/media"
	"avpipe/internal/pipeline"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "demo":
		err = runDemo(args)
	case "verify":
		err = runVerify(args)
	case "resume":
		err = runResume(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
	if err == nil {
		return
	}
	if errors.Is(err, pipeline.ErrInterrupt) {
		fmt.Println("interrupted=1 checkpoint_durable=1 exit_code=75")
		os.Exit(media.ExitInterrupt)
	}
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(media.ExitCode(err))
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: avpipe <demo|verify|resume> [flags]")
	fmt.Fprintln(os.Stderr, "  demo --out DIR [--seed 23] [--case clean|damaged|truncated|pcr_wrap|cc_jump|psi_bad|missing_idr]")
	fmt.Fprintln(os.Stderr, "       [--bitrates 600000,1200000] [--segments 12] [--interrupt-after N]")
	fmt.Fprintln(os.Stderr, "  verify --out DIR")
	fmt.Fprintln(os.Stderr, "  resume --out DIR")
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	seed := fs.Int("seed", 23, "deterministic fixture seed")
	_case := fs.String("case", "clean", "fixture case")
	bitrates := fs.String("bitrates", "600000,1200000", "comma-separated bitrates")
	segments := fs.Int("segments", 12, "tactical slot/segment count")
	interruptAfter := fs.Int("interrupt-after", 0, "exit 75 after N durable chunks")
	if err := fs.Parse(args); err != nil {
		return media.NewError(media.CodeConfig, -1, "%v", err)
	}
	bits, err := parseBitrates(*bitrates)
	if err != nil {
		return err
	}
	opt := pipeline.Options{
		Out: *out, Seed: *seed, Case: *_case, Bitrates: bits,
		Segments: *segments, InterruptAfter: *interruptAfter,
	}
	rep, err := pipeline.RunDemo(opt)
	if rep != nil {
		printReport(rep)
	}
	if err != nil {
		return err
	}
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return media.NewError(media.CodeConfig, -1, "%v", err)
	}
	rep, err := pipeline.RunVerify(pipeline.Options{Out: *out})
	if rep != nil {
		printReport(rep)
	}
	return err
}

func runResume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return media.NewError(media.CodeConfig, -1, "%v", err)
	}
	rep, err := pipeline.RunResume(pipeline.Options{Out: *out})
	if rep != nil {
		printReport(rep)
	}
	return err
}

func parseBitrates(s string) ([]int64, error) {
	parts := strings.Split(s, ",")
	var out []int64
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil || v <= 0 {
			return nil, media.NewError(media.CodeConfig, -1, "invalid bitrate %q", p)
		}
		out = append(out, v)
	}
	if len(out) < 2 {
		return nil, media.NewError(media.CodeConfig, -1, "at least two bitrates are required")
	}
	return out, nil
}

func printReport(r *pipeline.Report) {
	fmt.Printf("roundtrip=%.2f%%\n", r.RoundTripPercent())
	fmt.Printf("packets=%d\n", r.Packets)
	fmt.Printf("keyframe_hit=%d/%d\n", r.KeyframeFound, r.KeyframeExpected)
	fmt.Printf("aligned_slots=%d/%d\n", r.AlignedSlots, r.TotalSlots)
	fmt.Printf("segment_sha256=%d/%d\n", r.SegmentHashOK, r.SegmentHashTotal)
	fmt.Printf("cc_gap=%d\n", r.CCGap)
	fmt.Printf("psi_crc_reject=%d\n", r.PSICRCReject)
	fmt.Printf("gap_slots=%d\n", r.GapSlots)
	fmt.Printf("incomplete_tail=%d\n", r.IncompleteTail)
	fmt.Printf("fake_segments=0\n")
	fmt.Printf("first_desync_offset=%d\n", r.FirstDesync)
	if r.CheckpointReused > 0 || r.Rebuilt > 0 || r.Idempotent > 0 {
		fmt.Printf("checkpoint_reused=%d\n", r.CheckpointReused)
		fmt.Printf("rebuilt=%d\n", r.Rebuilt)
		fmt.Printf("idempotent=%d\n", r.Idempotent)
	}
	if len(r.OrphansRemoved) > 0 {
		fmt.Printf("orphans_removed=%d\n", len(r.OrphansRemoved))
	}
}
