package main

import (
	"fmt"

	"mp4replay/internal/mp4atom"
	"mp4replay/internal/report"
	"mp4replay/internal/seekidx"
)

func init() {
	commands["index"] = cmdIndex
	commands["seek"] = cmdSeek
}

func cmdIndex(args []string) (int, error) {
	fs := flagSet("index")
	fileP := fs.String("file", "", "input mp4")
	outP := fs.String("out", "", "output index json")
	if err := fs.Parse(args); err != nil {
		return report.ExitArgs, err
	}
	if *fileP == "" || *outP == "" {
		return report.ExitArgs, report.NewError(report.ExitArgs, "index",
			"requires --file and --out")
	}
	m, err := mp4atom.Open(*fileP)
	if err != nil {
		return 0, err
	}
	defer m.Close()
	tr := m.VideoTrack()
	if tr == nil {
		return report.ExitBoxInvalid, report.NewError(report.ExitBoxInvalid,
			"index", "no video track")
	}
	idx := seekidx.Build(tr)
	if err := idx.Save(*outP); err != nil {
		return 0, err
	}
	fmt.Printf("indexed %d samples, %d keyframes -> %s\n",
		idx.SampleCount, len(idx.Keys), *outP)
	return report.ExitOK, nil
}

func cmdSeek(args []string) (int, error) {
	fs := flagSet("seek")
	fileP := fs.String("file", "", "input mp4")
	atMs := fs.Int64("at-ms", 0, "target presentation time in ms")
	asJSON := fs.Bool("json", false, "emit JSON")
	indexP := fs.String("index", "", "optional saved index json")
	if err := fs.Parse(args); err != nil {
		return report.ExitArgs, err
	}
	if *fileP == "" {
		return report.ExitArgs, report.NewError(report.ExitArgs, "seek",
			"requires --file")
	}
	m, err := mp4atom.Open(*fileP)
	if err != nil {
		return 0, err
	}
	defer m.Close()
	tr := m.VideoTrack()
	if tr == nil {
		return report.ExitBoxInvalid, report.NewError(report.ExitBoxInvalid,
			"seek", "no video track")
	}
	var idx *seekidx.Index
	if *indexP != "" {
		idx, err = seekidx.Load(*indexP)
		if err != nil {
			return 0, err
		}
	} else {
		idx = seekidx.Build(tr)
	}
	land, err := idx.Locate(*atMs)
	if err != nil {
		return 0, err
	}
	res := report.SeekResult{
		RequestedMs: *atMs,
		Landing: report.SeekLanding{
			SampleIndex: land.Key.SampleIndex,
			PtsMs:       land.Key.PtsMs,
			IsSync:      true,
			Offset:      land.Key.Offset,
			Size:        land.Key.Size,
			FrameType:   land.FrameType,
		},
		GopDistanceMs: land.GopDistanceMs,
	}
	if *asJSON {
		b, _ := report.Marshal(res)
		fmt.Println(string(b))
	} else {
		fmt.Printf("sample=%d pts_ms=%d offset=%d size=%d gop_ms=%d\n",
			res.Landing.SampleIndex, res.Landing.PtsMs, res.Landing.Offset,
			res.Landing.Size, res.GopDistanceMs)
	}
	return report.ExitOK, nil
}
