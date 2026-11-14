package main

import (
	"encoding/json"
	"fmt"

	"mp4tool/internal/timeline"
)

type inspectOut struct {
	Boxes        int    `json:"boxes"`
	Traks        int    `json:"traks"`
	Timescale    uint32 `json:"timescale"`
	DurationMs   int64  `json:"duration_ms"`
	MoovOffset   uint64 `json:"moov_offset"`
	Samples      int    `json:"samples"`
	Keyframes    int    `json:"keyframes"`
	ElstSegments int    `json:"elst_segments"`
}

func cmdInspect(args []string) int {
	fs := newFlagSet("inspect")
	file := fs.String("file", "", "input mp4 file")
	asJSON := fs.Bool("json", false, "emit JSON")
	if fs.Parse(args) != nil || *file == "" {
		return exitUsage
	}
	p, err := load(*file, false)
	if err != nil {
		return fail(err)
	}
	defer p.close()
	out := inspectOut{
		Boxes:      p.moov.BoxCount,
		Traks:      len(p.moov.Traks),
		Timescale:  p.moov.Timescale,
		MoovOffset: p.moov.Offset,
	}
	out.DurationMs = timeline.TicksToMs(int64(p.moov.Duration), p.moov.Timescale)
	if p.trak != nil {
		out.Samples = len(p.trak.Table.Sizes)
		out.Keyframes = len(p.trak.Table.Stss)
		if out.Keyframes == 0 {
			out.Keyframes = out.Samples // no stss: all samples are keyframes
		}
		out.ElstSegments = len(p.trak.Elst)
	}
	if *asJSON {
		data, _ := json.Marshal(out)
		fmt.Println(string(data))
	} else {
		fmt.Printf("boxes=%d traks=%d timescale=%d duration_ms=%d moov_offset=%d samples=%d keyframes=%d elst_segments=%d\n",
			out.Boxes, out.Traks, out.Timescale, out.DurationMs, out.MoovOffset,
			out.Samples, out.Keyframes, out.ElstSegments)
	}
	return exitOK
}
