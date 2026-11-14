package main

import (
	"encoding/json"
	"fmt"

	"mp4tool/internal/index"
	"mp4tool/internal/sample"
	"mp4tool/internal/timeline"
)

type seekOut struct {
	TargetMs     int64  `json:"target_ms"`
	LandingPtsMs int64  `json:"landing_pts_ms"`
	LandingDtsMs int64  `json:"landing_dts_ms"`
	DeltaMs      int64  `json:"delta_ms"`
	FrameType    string `json:"frame_type"`
	SampleIndex  int    `json:"sample_index"`
	RawPts       int64  `json:"raw_pts"`
	NormPtsMs    int64  `json:"norm_pts_ms"`
	SHA256       string `json:"sha256"`
}

func cmdSeek(args []string) int {
	fs := newFlagSet("seek")
	file := fs.String("file", "", "input mp4 file")
	ms := fs.Int64("ms", -1, "target presentation time in milliseconds")
	exact := fs.Bool("exact", false, "allow landing on non-keyframe")
	asJSON := fs.Bool("json", false, "emit JSON")
	if fs.Parse(args) != nil || *file == "" || *ms < 0 {
		return exitUsage
	}
	p, err := load(*file, true)
	if err != nil {
		return fail(err)
	}
	defer p.close()
	ts := p.trak.Timescale
	frame, err := index.Locate(p.frames, timeline.MsToTicks(*ms, ts), *exact)
	if err != nil {
		return fail(err)
	}
	sum, err := sample.HashSample(p.f, frame) // reads exactly one sample
	if err != nil {
		return fail(err)
	}
	typ := "P"
	if frame.Keyframe {
		typ = "I"
	}
	out := seekOut{
		TargetMs:     *ms,
		LandingPtsMs: timeline.TicksToMs(frame.NormPTS, ts),
		LandingDtsMs: timeline.TicksToMs(frame.DTS, ts),
		FrameType:    typ,
		SampleIndex:  frame.Index,
		RawPts:       frame.PTS,
		NormPtsMs:    timeline.TicksToMs(frame.NormPTS, ts),
		SHA256:       sum,
	}
	out.DeltaMs = out.LandingPtsMs - out.TargetMs
	if *asJSON {
		data, _ := json.Marshal(out)
		fmt.Println(string(data))
	} else {
		fmt.Printf("target_ms=%d landing_pts_ms=%d landing_dts_ms=%d delta_ms=%d frame_type=%s sample_index=%d raw_pts=%d norm_pts_ms=%d sha256=%s\n",
			out.TargetMs, out.LandingPtsMs, out.LandingDtsMs, out.DeltaMs,
			out.FrameType, out.SampleIndex, out.RawPts, out.NormPtsMs, out.SHA256)
	}
	return exitOK
}
