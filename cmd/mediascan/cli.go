package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// parseArgs turns os.Args into Options. Only the "demo" subcommand exists.
func parseArgs(args []string) (Options, error) {
	o := defaultOptions()
	if len(args) < 2 || args[1] != "demo" {
		return o, fmt.Errorf("usage: mediascan demo [flags]")
	}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Int64Var(&o.Seed, "seed", o.Seed, "deterministic generator seed")
	fs.IntVar(&o.Frames, "frames", o.Frames, "frame count")
	fs.IntVar(&o.FPS, "fps", o.FPS, "frame rate")
	fs.IntVar(&o.GOP, "gop", o.GOP, "keyframe interval in frames")
	fs.IntVar(&o.TargetKbps, "target-kbps", o.TargetKbps, "configured target bitrate kbps")
	fs.IntVar(&o.ChunkFrames, "chunk-frames", o.ChunkFrames, "frames per committed chunk")
	fs.StringVar(&o.BandwidthTrace, "bandwidth-trace", o.BandwidthTrace, "bandwidth trace source")
	fs.StringVar(&o.Out, "out", o.Out, "output directory")
	fs.IntVar(&o.InterruptAfter, "interrupt-after", o.InterruptAfter, "simulate interrupt after N chunks (-1 off)")
	var corrupt string
	fs.StringVar(&corrupt, "corrupt", "", "comma list: checksum,truncate-box")
	fs.BoolVar(&o.Resume, "resume", false, "resume an interrupted run")
	fs.BoolVar(&o.RebuildIndex, "rebuild-index", false, "rebuild index after truncation")
	if err := fs.Parse(args[2:]); err != nil {
		return o, err
	}
	if corrupt != "" {
		for _, c := range strings.Split(corrupt, ",") {
			c = strings.TrimSpace(c)
			switch c {
			case "checksum", "truncate-box":
				o.Corrupt = append(o.Corrupt, c)
			default:
				return o, fmt.Errorf("unknown corruption kind %q", c)
			}
		}
	}
	if err := o.validate(); err != nil {
		return o, err
	}
	return o, nil
}

func (o Options) validate() error {
	if o.Seed < 0 {
		return fmt.Errorf("seed must be non-negative")
	}
	if o.Frames <= 0 || o.FPS <= 0 || o.GOP <= 0 || o.TargetKbps <= 0 || o.ChunkFrames <= 0 {
		return fmt.Errorf("frames/fps/gop/target/chunk must be positive")
	}
	if o.ChunkFrames > o.Frames {
		return fmt.Errorf("chunk-frames cannot exceed frames")
	}
	if o.ReorderWindow == 0 || o.ReorderWindow > rtpWindowCap {
		return fmt.Errorf("reorder window outside (0,%d]", rtpWindowCap)
	}
	if strings.Contains(o.Out, "..") {
		return fmt.Errorf("output path must not contain '..'")
	}
	if o.BandwidthTrace != "built-in" {
		return fmt.Errorf("only built-in bandwidth trace is supported")
	}
	return nil
}

// totalChunks computes the ceil(frames/chunk) chunk count.
func (o Options) totalChunks() int {
	return (o.Frames + o.ChunkFrames - 1) / o.ChunkFrames
}

// itoa avoids importing strconv scattered across print sites.
func itoa(n int) string { return strconv.Itoa(n) }
