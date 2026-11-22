// Command medialine is the terminal entry point for the batch quality-check and
// repackaging kernel. It exposes two subcommands:
//
//	medialine demo   --seed 17 --tracks 4 --bitrates 3 --segment-us 2000000 \
//	                 --queue 4 --workers 3 --corrupt tail --cancel-after 5 \
//	                 --out ./tmp-run
//	medialine verify --out ./tmp-run
//
// Exit codes: 0 success, 2 input was damaged but usable fragments were
// produced under the recovery policy, 1 bad parameters or an unrecoverable
// write error.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const (
	exitOK      = 0
	exitFatal   = 1
	exitPartial = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return exitFatal
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "demo":
		return runDemo(ctx, args[1:])
	case "verify":
		return runVerify(args[1:])
	case "-h", "--help", "help":
		usage(os.Stdout)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", args[0])
		usage(os.Stderr)
		return exitFatal
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `medialine - multi-track quality verification and segment repackaging

Usage:
  medialine demo   [options]   generate fixtures, run the pipeline, print metrics
  medialine verify [options]   re-check every delivered fragment

Demo options:
  --seed int          fixture seed (default 17)
  --tracks int        tracks per representation (default 4)
  --bitrates int      bitrate ladder rungs (default 3)
  --segment-us int    nominal segment length in microseconds (default 2000000)
  --queue int         worker submission queue capacity (default 4)
  --workers int       concurrent workers (default 3)
  --corrupt string    source damage: none|tail|length|sample (default none)
  --cancel-after int  cancel after this many completed segments, 0 disables
  --out string        output directory (required)
  --duration-us int   source duration per clip (default 12000000)
  --cut-us int        operator edit point; repeat for several cuts

Verify options:
  --out string        directory containing manifest.json (required)

Exit codes: 0 ok, 2 damaged input but usable output, 1 fatal.
`)
}

// demoConfig is the validated demo invocation.
type demoConfig struct {
	Seed        int64
	Tracks      int
	Bitrates    int
	SegmentUS   int64
	Queue       int
	Workers     int
	Corrupt     string
	CancelAfter int
	Out         string
	DurationUS  int64
	// Cuts are operator edit points on the unified timeline, in microseconds.
	Cuts []int64
}

func parseDemoFlags(args []string) (demoConfig, error) {
	var cfg demoConfig
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Int64Var(&cfg.Seed, "seed", 17, "fixture seed")
	fs.IntVar(&cfg.Tracks, "tracks", 4, "tracks per representation")
	fs.IntVar(&cfg.Bitrates, "bitrates", 3, "bitrate ladder rungs")
	fs.Int64Var(&cfg.SegmentUS, "segment-us", 2_000_000, "nominal segment length (microseconds)")
	fs.IntVar(&cfg.Queue, "queue", 4, "submission queue capacity")
	fs.IntVar(&cfg.Workers, "workers", 3, "concurrent workers")
	fs.StringVar(&cfg.Corrupt, "corrupt", "none", "source damage: none|tail|length|sample")
	fs.IntVar(&cfg.CancelAfter, "cancel-after", 0, "cancel after N completed segments")
	fs.StringVar(&cfg.Out, "out", "", "output directory")
	fs.Int64Var(&cfg.DurationUS, "duration-us", 12_000_000, "source duration per clip")
	fs.Var((*int64List)(&cfg.Cuts), "cut-us", "repeatable operator edit point in microseconds")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c demoConfig) validate() error {
	switch {
	case c.Seed < 0:
		return fmt.Errorf("seed must not be negative")
	case c.Tracks < 1 || c.Tracks > 64:
		return fmt.Errorf("tracks must be between 1 and 64")
	case c.Bitrates < 1 || c.Bitrates > 8:
		return fmt.Errorf("bitrates must be between 1 and 8")
	case c.SegmentUS < 100_000:
		return fmt.Errorf("segment-us must be at least 100000")
	case c.SegmentUS > 60_000_000:
		return fmt.Errorf("segment-us must be at most 60000000")
	case c.Queue < 0 || c.Queue > 4096:
		return fmt.Errorf("queue must be between 0 and 4096")
	case c.Workers < 1 || c.Workers > 256:
		return fmt.Errorf("workers must be between 1 and 256")
	case c.CancelAfter < 0:
		return fmt.Errorf("cancel-after must not be negative")
	case c.DurationUS < c.SegmentUS:
		return fmt.Errorf("duration-us must be at least one segment")
	case c.Out == "":
		return fmt.Errorf("--out is required")
	case filepath.Clean(c.Out) == "/":
		return fmt.Errorf("refusing to use / as output directory")
	}
	switch c.Corrupt {
	case "none", "tail", "length", "sample":
	default:
		return fmt.Errorf("corrupt must be one of none|tail|length|sample")
	}
	prev := int64(-1)
	for _, cut := range c.Cuts {
		if cut <= 0 || cut >= c.DurationUS {
			return fmt.Errorf("cut-us %d must lie strictly inside the timeline", cut)
		}
		if cut <= prev {
			return fmt.Errorf("cut-us values must be increasing and unique")
		}
		prev = cut
	}
	return nil
}

// int64List collects repeatable numeric flags.
type int64List []int64

func (l *int64List) String() string { return fmt.Sprint([]int64(*l)) }

func (l *int64List) Set(v string) error {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid --cut-us %q: %w", v, err)
	}
	*l = append(*l, n)
	return nil
}

func parseVerifyFlags(args []string) (string, error) {
	var out string
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&out, "out", "", "directory containing manifest.json")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("--out is required")
	}
	return out, nil
}

func formatDuration(d time.Duration) string { return d.Round(time.Millisecond).String() }
