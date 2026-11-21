package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// options are the demo's command-line settings.
type options struct {
	Seed        int64
	Rates       []int
	SegmentTick int64
	FrameCount  int
	Inject      string
	Resume      bool
	WorkDir     string
	Policies    policies
}

// policies are optional explicit conflict decisions.  The zero values block,
// which is the safe default the specification requires.
type policies struct {
	Audio string
	Color string
}

func parseDemo(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Int64Var(&opts.Seed, "seed", 41, "deterministic fixture seed")
	rates := fs.String("rates", "800000,1600000", "comma-separated bitrates")
	fs.Int64Var(&opts.SegmentTick, "segment-ticks", 90000, "segment length in programme ticks")
	fs.IntVar(&opts.FrameCount, "frames", 90, "frames per representation")
	fs.StringVar(&opts.Inject, "inject", "", "fault to inject")
	fs.BoolVar(&opts.Resume, "resume", false, "resume interrupted work from the persisted state")
	fs.StringVar(&opts.WorkDir, "workdir", ".mediapack/demo", "directory for fixtures and state")
	fs.StringVar(&opts.Policies.Audio, "audio-policy", "", "audio conflict policy: block, prefer:<source>, downmix:<layout>")
	fs.StringVar(&opts.Policies.Color, "color-policy", "", "colour conflict policy: block or prefer:<source>")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if opts.Seed < 0 {
		return opts, errors.New("seed must not be negative")
	}
	if opts.FrameCount < 1 || opts.FrameCount > 3600 {
		return opts, errors.New("frames must be between 1 and 3600")
	}
	if opts.SegmentTick <= 0 {
		return opts, errors.New("segment-ticks must be positive")
	}
	parsed, err := parseRates(*rates)
	if err != nil {
		return opts, err
	}
	opts.Rates = parsed
	if err := validateInject(opts.Inject); err != nil {
		return opts, err
	}
	if strings.TrimSpace(opts.WorkDir) == "" {
		return opts, errors.New("workdir must not be empty")
	}
	if err := validatePolicy("audio", opts.Policies.Audio); err != nil {
		return opts, err
	}
	if err := validatePolicy("color", opts.Policies.Color); err != nil {
		return opts, err
	}
	return opts, nil
}

func parseRates(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	if len(parts) == 0 || len(parts) > 8 {
		return nil, errors.New("rates must list between 1 and 8 bitrates")
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || v <= 0 || v > 1_000_000_000 {
			return nil, fmt.Errorf("invalid bitrate %q", p)
		}
		if seen[v] {
			return nil, fmt.Errorf("duplicate bitrate %d", v)
		}
		seen[v] = true
		out = append(out, v)
	}
	return out, nil
}

var injectable = map[string]bool{
	"": true, "corrupt-frame": true, "duplicate-frame": true, "missing-frame": true,
	"truncated-mp4": true, "illegal-box": true, "audio-conflict": true,
	"color-conflict": true, "misaligned-rate": true, "unknown-box": true,
}

func validateInject(s string) error {
	if !injectable[s] {
		return fmt.Errorf("unknown injection %q", s)
	}
	return nil
}

// validatePolicy accepts the documented policy grammar so that a typo fails
// as a usage error (exit 1) before any media work starts.
func validatePolicy(kind, s string) error {
	if s == "" || s == "block" {
		return nil
	}
	verb, arg, ok := strings.Cut(s, ":")
	if !ok || strings.TrimSpace(arg) == "" {
		return fmt.Errorf("%s policy %q must be block, prefer:<source> or downmix:<layout>", kind, s)
	}
	switch verb {
	case "prefer":
		return nil
	case "downmix":
		if kind != "audio" {
			return fmt.Errorf("downmix is only valid for audio")
		}
		return nil
	default:
		return fmt.Errorf("unknown %s policy %q", kind, s)
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: mediapack <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "commands:")
	fmt.Fprintln(w, "  demo   run the packaging demonstration")
	fmt.Fprintln(w, "  help   print this message")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "demo flags:")
	fmt.Fprintln(w, "  --seed N               fixture seed (default 41)")
	fmt.Fprintln(w, "  --rates a,b            bitrate representations (default 800000,1600000)")
	fmt.Fprintln(w, "  --segment-ticks N      segment length in programme ticks (default 90000)")
	fmt.Fprintln(w, "  --frames N             frames per representation (default 90)")
	fmt.Fprintln(w, "  --inject NAME          inject a deterministic fault")
	fmt.Fprintln(w, "  --resume               resume from persisted state")
	fmt.Fprintln(w, "  --workdir DIR          fixture and state directory (default .mediapack/demo)")
	fmt.Fprintln(w, "  --audio-policy P       block, prefer:<source>, downmix:<layout>")
	fmt.Fprintln(w, "  --color-policy P       block or prefer:<source>")
}
