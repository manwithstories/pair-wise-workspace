package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"pairflow/internal/model"
	"pairflow/internal/pipeline"
)

func main() {
	os.Exit(runWithRecovery(os.Args, os.Stdout, os.Stderr))
}

func runWithRecovery(args []string, stdout, stderr io.Writer) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "internal error: %v\n", r)
			code = model.ExitInternal
		}
	}()
	return run(args, stdout, stderr)
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 || args[1] != "prove" {
		fmt.Fprintln(stderr, "usage: mediasync prove --work DIR [flags]")
		return model.ExitUsage
	}
	fs := flag.NewFlagSet("prove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	work := fs.String("work", "", "proof working directory")
	seed := fs.Int64("seed", 1, "deterministic generator seed")
	tracks := fs.Int("tracks", 4, "video track count")
	frames := fs.Int("frames-per-track", 1200, "frames per track")
	payload := fs.Int("payload-bytes", 4096, "frame payload bytes")
	drift := fs.Float64("drift-ppm", 0, "clock drift in PPM")
	rollover := fs.Uint64("rollover", 0, "initial RTP timestamp near 32-bit wrap")
	vfrSeed := fs.Int64("vfr-seed", 1, "variable frame-rate seed")
	dropSeqs := fs.String("drop-seqs", "", "comma-separated RTP sequences to drop")
	damageTrack := fs.String("damage-track", "", "track containing injected damage")
	damageFrame := fs.Int64("damage-frame", 0, "frame containing injected damage")
	corruptByte := fs.Int("corrupt-byte", 0, "payload byte to corrupt")
	crashAfter := fs.Int("crash-after-records", 0, "simulate crash after N output records")
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return model.ExitOK
		}
		return model.ExitUsage
	}

	drops, err := parseSeqs(*dropSeqs)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return model.ExitUsage
	}
	cfg := pipeline.Config{
		WorkDir:           *work,
		Seed:              *seed,
		Tracks:            *tracks,
		FramesPerTrack:    *frames,
		PayloadBytes:      *payload,
		DriftPPM:          *drift,
		Rollover:          uint32(*rollover),
		VFRSeed:           *vfrSeed,
		DropSeqs:          drops,
		DamageTrack:       *damageTrack,
		DamageFrame:       *damageFrame,
		CorruptByte:       *corruptByte,
		CrashAfterRecords: *crashAfter,
	}
	if cfg.WorkDir == "" {
		fmt.Fprintln(stderr, "error: --work is required")
		return model.ExitUsage
	}
	report, err := pipeline.Prove(cfg, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return model.ExitCode(err)
	}
	_ = report
	return model.ExitOK
}

func parseSeqs(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 || n > 65535 {
			return nil, &model.MediaError{Code: model.CodeInvalidArgument, Message: "invalid --drop-seqs value: " + p}
		}
		out = append(out, n)
	}
	return out, nil
}
