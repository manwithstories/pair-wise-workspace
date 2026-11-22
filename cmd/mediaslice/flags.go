package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type demoFlags struct {
	seed            int64
	duration        time.Duration
	bitrates        []string
	segment         time.Duration
	inject          []string
	checkpointEvery time.Duration
	resume          bool
	workDir         string
}

func parseDemoFlags(args []string) (*demoFlags, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	d := &demoFlags{}
	var bitrates, inject, durationRaw, segmentRaw, checkpointRaw string
	fs.Int64Var(&d.seed, "seed", 42, "deterministic fixture seed")
	fs.StringVar(&durationRaw, "duration", "1800s", "fixture duration")
	fs.StringVar(&bitrates, "bitrates", "800k,1600k,3200k", "comma-separated bitrate ids")
	fs.StringVar(&segmentRaw, "segment", "2s", "target segment duration")
	fs.StringVar(&inject, "inject", "", "comma-separated fault injections")
	fs.StringVar(&checkpointRaw, "checkpoint-every", "20s", "checkpoint cadence")
	fs.BoolVar(&d.resume, "resume", false, "simulate interruption then resume")
	fs.StringVar(&d.workDir, "work-dir", "mediaslice-out", "output working directory")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	var err error
	if d.duration, err = parseDuration(durationRaw); err != nil {
		return nil, fmt.Errorf("duration: %w", err)
	}
	if d.segment, err = parseDuration(segmentRaw); err != nil {
		return nil, fmt.Errorf("segment: %w", err)
	}
	if d.checkpointEvery, err = parseDuration(checkpointRaw); err != nil {
		return nil, fmt.Errorf("checkpoint-every: %w", err)
	}
	for _, b := range strings.Split(bitrates, ",") {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		if _, err := parseBitrate(b); err != nil {
			return nil, err
		}
		d.bitrates = append(d.bitrates, b)
	}
	if len(d.bitrates) == 0 {
		return nil, fmt.Errorf("at least one bitrate required")
	}
	for _, inj := range strings.Split(inject, ",") {
		inj = strings.TrimSpace(inj)
		if inj != "" {
			d.inject = append(d.inject, inj)
		}
	}
	return d, nil
}

func parseDuration(raw string) (time.Duration, error) {
	if !strings.HasSuffix(raw, "s") {
		return 0, fmt.Errorf("duration %q must use seconds suffix (e.g. 2s)", raw)
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if v <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return v, nil
}

// parseBitrate accepts 800k / 1.6m style ids and returns bits per second.
func parseBitrate(raw string) (int, error) {
	if raw == "" {
		return 0, fmt.Errorf("empty bitrate")
	}
	mult := 1
	num := raw
	switch raw[len(raw)-1] {
	case 'k', 'K':
		mult = 1000
		num = raw[:len(raw)-1]
	case 'm', 'M':
		mult = 1_000_000
		num = raw[:len(raw)-1]
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid bitrate %q", raw)
	}
	return int(v * float64(mult)), nil
}
