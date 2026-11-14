package main

import (
	"encoding/json"
	"fmt"

	"mp4tool/internal/sample"
	"mp4tool/internal/timeline"
)

type hashOut struct {
	Index     int    `json:"index"`
	DtsMs     int64  `json:"dts_ms"`
	PtsMs     int64  `json:"pts_ms"`
	NormPtsMs int64  `json:"norm_pts_ms"`
	Offset    uint64 `json:"offset"`
	Size      uint32 `json:"size"`
	Keyframe  bool   `json:"keyframe"`
	SHA256    string `json:"sha256"`
}

func cmdHash(args []string) int {
	fs := newFlagSet("hash")
	file := fs.String("file", "", "input mp4 file")
	head := fs.Int("head", 0, "only hash the first N samples")
	if fs.Parse(args) != nil || *file == "" || *head < 0 {
		return exitUsage
	}
	p, err := load(*file, true)
	if err != nil {
		return fail(err)
	}
	defer p.close()
	ts := p.trak.Timescale
	n := len(p.frames)
	if *head > 0 && *head < n {
		n = *head
	}
	for i := 0; i < n; i++ {
		f := p.frames[i]
		sum, err := sample.HashSample(p.f, f)
		if err != nil {
			return fail(err)
		}
		out := hashOut{
			Index: f.Index, DtsMs: timeline.TicksToMs(f.DTS, ts),
			PtsMs: timeline.TicksToMs(f.PTS, ts), NormPtsMs: timeline.TicksToMs(f.NormPTS, ts),
			Offset: f.Offset, Size: f.Size, Keyframe: f.Keyframe, SHA256: sum,
		}
		data, _ := json.Marshal(out)
		fmt.Println(string(data))
	}
	return exitOK
}
