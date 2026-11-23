// Package cli holds the demo's flag parsing and validation so the command entry
// point stays a thin shell and the rules can be tested without a process.
package cli

import (
	"flag"
	"fmt"
	"io"
)

// DemoFlags is the parsed flag set for `mediasync demo`.
type DemoFlags struct {
	Seed              int64
	LossBurst         int
	Duplicate         int
	Reorder           int
	BandwidthKbps     int
	TargetKbps        int
	CheckpointSamples int
	Resume            bool
	Corrupt           string
	Rotation          int
	// RotationSet records whether the operator supplied --rotation, so a
	// default zero is distinguishable from an explicit 0.
	RotationSet bool
	WorkDir     string
	Source      string
}

// ParseDemo reads the demo flags. Errors are returned rather than exiting.
func ParseDemo(args []string, stderr io.Writer) (*DemoFlags, error) {
	f := &DemoFlags{}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Int64Var(&f.Seed, "seed", 41, "fixture seed")
	fs.IntVar(&f.LossBurst, "loss-burst", 7, "RTP loss burst length")
	fs.IntVar(&f.Duplicate, "duplicate", 3, "RTP duplicate count")
	fs.IntVar(&f.Reorder, "reorder", 5, "RTP reorder distance")
	fs.IntVar(&f.BandwidthKbps, "bandwidth-kbps", 1800, "seed bandwidth estimate")
	fs.IntVar(&f.TargetKbps, "target-kbps", 2000, "declared bitrate target")
	fs.IntVar(&f.CheckpointSamples, "checkpoint-samples", 120, "samples per block")
	fs.BoolVar(&f.Resume, "resume", false, "interrupt once then resume")
	fs.StringVar(&f.Corrupt, "corrupt", "", "damaged input fixture")
	fs.IntVar(&f.Rotation, "rotation", 0, "explicit rotation override")
	fs.StringVar(&f.WorkDir, "work-dir", "", "working directory (default: temp)")
	fs.StringVar(&f.Source, "source", "", "force container reader: mp4 or mxf")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "rotation" {
			f.RotationSet = true
		}
	})
	return f, nil
}

// Validate rejects impossible flag combinations before any work is done.
func (f *DemoFlags) Validate() error {
	if f.LossBurst < 0 || f.Duplicate < 0 || f.Reorder < 0 {
		return fmt.Errorf("impairment counts cannot be negative")
	}
	if f.BandwidthKbps <= 0 || f.TargetKbps <= 0 {
		return fmt.Errorf("bandwidth and target must be positive")
	}
	if f.CheckpointSamples <= 0 {
		return fmt.Errorf("checkpoint-samples must be positive")
	}
	if f.RotationSet {
		switch f.Rotation {
		case 0, 90, 180, 270:
		default:
			return fmt.Errorf("rotation must be 0, 90, 180 or 270")
		}
	}
	switch f.Corrupt {
	case "", "truncated-mp4", "invalid-klv":
	default:
		return fmt.Errorf("unknown corrupt fixture %q", f.Corrupt)
	}
	switch f.Source {
	case "", "mp4", "mxf":
	default:
		return fmt.Errorf("unknown source %q", f.Source)
	}
	return nil
}
