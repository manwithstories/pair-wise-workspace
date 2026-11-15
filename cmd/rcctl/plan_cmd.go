package main

import (
	"context"
	"fmt"
	"io"

	"rcctl/internal/plan"
	"rcctl/internal/runner"
)

func commandPlan(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("plan", stderr)
	clip := fs.String("clip", "news", "synthetic clip name")
	frames := fs.Int("frames", 240, "frame count (zero is allowed)")
	gop := fs.Int("gop", 20, "frames per segment")
	target := fs.Float64("target-kbps", 1200, "clip average target bitrate")
	out := fs.String("out", "work/plan.json", "plan output path")
	fps := fs.Int("fps", 30, "frame rate")
	width := fs.Int("width", 176, "frame width (multiple of 16)")
	height := fs.Int("height", 144, "frame height (multiple of 16)")
	seed := fs.Int64("seed", 10952, "deterministic source seed")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	if *frames < 0 || *gop <= 0 || *fps <= 0 || *target <= 0 || *clip == "" {
		return failInvalid(stderr, fmt.Errorf("frames must be >=0; gop, fps, target-kbps, and clip are required"))
	}
	p := plan.New(*clip, *frames, *width, *height, *fps, *gop, *target, *seed)
	if err := p.Validate(); err != nil {
		return failInvalid(stderr, err)
	}
	if err := runner.BuildPlan(context.Background(), p); err != nil {
		return failInvalid(stderr, fmt.Errorf("first pass: %w", err))
	}
	if *frames == 0 {
		fmt.Fprintln(stderr, "warning: zero frames requested; plan contains no segments")
	}
	if len(p.Segments) > 0 && p.Segments[len(p.Segments)-1].Frames < *gop {
		fmt.Fprintf(stderr, "warning: final segment has %d frames, less than one GOP (%d)\n",
			p.Segments[len(p.Segments)-1].Frames, *gop)
	}
	if err := plan.Save(*out, p); err != nil {
		fmt.Fprintf(stderr, "rcctl: write plan: %v\n", err)
		return exitRuntime
	}
	fmt.Fprintf(stdout, "plan{clip:%s, frames:%d, segments:%d, gop:%d, target_kbps:%.2f, plan_id:%s}\n",
		p.Clip, p.Frames, len(p.Segments), p.GOP, p.TargetKbps, p.PlanID)
	fmt.Fprintln(stdout, "segments[]{index,start_frame,end_frame,target_kbps,complexity,hard_cut}")
	for _, s := range p.Segments {
		fmt.Fprintf(stdout, "  {%d,%d,%d,%.2f,%.2f,%t}\n", s.Index, s.StartFrame, s.EndFrame,
			s.TargetKbps, s.Complexity, s.HardCut)
	}
	fmt.Fprintf(stdout, "wrote %s\n", *out)
	return exitOK
}
