// Command mediacheck is the terminal acceptance entry point. It generates
// deterministic synthetic assets, runs the healthy/damaged/recovery paths
// and prints the required acceptance fields. No GUI, no network, no real
// program material: every input is self-generated in the work directory.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Exit codes follow the acceptance contract.
const (
	exitOK              = 0
	exitBadArgs         = 2
	exitUnrecoverable   = 3
	exitIndexFailure    = 4
)

// DemoArgs is the parsed demo command line.
type DemoArgs struct {
	Seed             int64
	DurationSec      int64
	Representations  []string
	SegmentSec       int
	CheckpointEvery  int
	LossBurst        int
	TruncateTail     int
	WorkDir          string
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: mediacheck demo [flags]")
		return exitBadArgs
	}
	d, err := parseDemoArgs(args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid arguments: %v\n", sanitize(err.Error()))
		return exitBadArgs
	}
	if err := d.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid input: %v\n", sanitize(err.Error()))
		return exitBadArgs
	}
	code, err := executeDemo(d)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo failed: %v\n", sanitize(err.Error()))
	}
	return code
}

func parseDemoArgs(args []string) (DemoArgs, error) {
	d := DemoArgs{
		SegmentSec: 6, CheckpointEvery: 256, LossBurst: 12,
		TruncateTail: 1, Representations: []string{"360p", "720p", "1080p"},
		WorkDir: filepath.Join(os.TempDir(), "mediacheck-demo"),
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			return d, fmt.Errorf("unexpected argument %q", a)
		}
		key := strings.TrimPrefix(a, "--")
		if i+1 >= len(args) {
			return d, fmt.Errorf("flag --%s needs a value", key)
		}
		val := args[i+1]
		i++
		if err := d.apply(key, val); err != nil {
			return d, err
		}
	}
	return d, nil
}

func (d *DemoArgs) apply(key, val string) error {
	switch key {
	case "seed":
		v, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return fmt.Errorf("--seed: %w", err)
		}
		d.Seed = v
	case "duration":
		secs, err := parseDurationSeconds(val)
		if err != nil {
			return fmt.Errorf("--duration: %w", err)
		}
		d.DurationSec = secs
	case "representations":
		d.Representations = strings.Split(val, ",")
	case "segment":
		secs, err := parseDurationSeconds(val)
		if err != nil {
			return fmt.Errorf("--segment: %w", err)
		}
		d.SegmentSec = int(secs)
	case "checkpoint-every":
		v, err := strconv.Atoi(val)
		if err != nil || v <= 0 {
			return fmt.Errorf("--checkpoint-every must be positive, got %q", val)
		}
		d.CheckpointEvery = v
	case "loss-burst":
		v, err := strconv.Atoi(val)
		if err != nil || v < 0 {
			return fmt.Errorf("--loss-burst must be non-negative, got %q", val)
		}
		d.LossBurst = v
	case "truncate-tail":
		v, err := strconv.Atoi(val)
		if err != nil || v < 0 || v > 1 {
			return fmt.Errorf("--truncate-tail must be 0 or 1, got %q", val)
		}
		d.TruncateTail = v
	case "work-dir":
		d.WorkDir = val
	default:
		return fmt.Errorf("unknown flag --%s", key)
	}
	return nil
}

func (d DemoArgs) validate() error {
	if d.DurationSec <= 0 {
		return errors.New("duration must be positive")
	}
	if d.SegmentSec <= 0 {
		return errors.New("segment duration must be positive")
	}
	if len(d.Representations) == 0 {
		return errors.New("at least one representation is required")
	}
	for _, r := range d.Representations {
		if strings.TrimSpace(r) == "" {
			return errors.New("representation name is empty")
		}
	}
	return nil
}

// parseDurationSeconds accepts "7200s" or "7200".
func parseDurationSeconds(val string) (int64, error) {
	val = strings.TrimSuffix(val, "s")
	secs, err := strconv.ParseInt(val, 10, 64)
	if err != nil || secs <= 0 {
		return 0, fmt.Errorf("invalid duration %q", val)
	}
	return secs, nil
}

// sanitize keeps host-sensitive absolute paths out of error output.
func sanitize(msg string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		msg = strings.ReplaceAll(msg, home, "~")
	}
	return msg
}
