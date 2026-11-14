package main

import (
	"encoding/json"
	"fmt"
	"os"

	"mp4replay/internal/mp4atom"
	"mp4replay/internal/remux"
	"mp4replay/internal/report"
	"mp4replay/internal/seekidx"
)

// Target is one entry of the JSON array consumed by --targets.
type Target struct {
	AtMs   int64  `json:"at_ms"`
	Frames int    `json:"frames"`
	Out    string `json:"out"`
}

// BatchResult summarizes one exported target.
type BatchResult struct {
	Out         string `json:"out"`
	SampleStart int    `json:"sample_start"`
	SampleEnd   int    `json:"sample_end"`
	Bytes       int64  `json:"bytes"`
	Status      string `json:"status"`
}

func runBatch(file string, targets []Target) ([]BatchResult, error) {
	m, err := mp4atom.Open(file)
	if err != nil {
		return nil, err
	}
	defer m.Close()
	vtr := m.VideoTrack()
	if vtr == nil {
		return nil, report.NewError(report.ExitBoxInvalid, "batch",
			"no video track")
	}
	idx := seekidx.Build(vtr)
	results := make([]BatchResult, 0, len(targets))
	for _, tg := range targets {
		land, err := idx.Locate(tg.AtMs)
		if err != nil {
			return nil, err
		}
		start := land.Key.SampleIndex
		end := start + tg.Frames
		if end > len(vtr.Samples) {
			end = len(vtr.Samples)
		}
		spans := []remux.Span{{Track: vtr, Start: start, End: end}}
		if atr := m.AudioTrack(); atr != nil {
			aStart, aEnd := audioRange(atr, vtr, start, end)
			if aEnd > aStart {
				spans = append(spans, remux.Span{
					Track: atr, Start: aStart, End: aEnd,
				})
			}
		}
		if err := remux.Export(m, spans, tg.Out); err != nil {
			return nil, err
		}
		results = append(results, BatchResult{
			Out: tg.Out, SampleStart: start, SampleEnd: end,
			Bytes: exportedBytes(tg.Out), Status: "ok",
		})
	}
	return results, nil
}

func exportedBytes(path string) int64 {
	if st, err := os.Stat(path); err == nil {
		return st.Size()
	}
	return 0
}

func loadTargets(path string) ([]Target, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, report.NewError(report.ExitFileRead, "targets", err.Error())
	}
	var ts []Target
	if err := json.Unmarshal(b, &ts); err != nil {
		return nil, report.NewError(report.ExitArgs, "targets", err.Error())
	}
	if len(ts) == 0 {
		return nil, report.NewError(report.ExitArgs, "targets",
			"empty targets array")
	}
	return ts, nil
}

func init() {
	commands["batch"] = func(args []string) (int, error) {
		fs := flagSet("batch")
		fileP := fs.String("file", "", "input mp4")
		targetsP := fs.String("targets", "", "JSON array file")
		asJSON := fs.Bool("json", false, "emit JSON")
		if err := fs.Parse(args); err != nil {
			return report.ExitArgs, err
		}
		if *fileP == "" || *targetsP == "" {
			return report.ExitArgs, report.NewError(report.ExitArgs, "batch",
				"requires --file and --targets")
		}
		targets, err := loadTargets(*targetsP)
		if err != nil {
			return report.ExitArgs, err
		}
		results, err := runBatch(*fileP, targets)
		if err != nil {
			return 0, err
		}
		if *asJSON {
			b, _ := report.Marshal(results)
			fmt.Println(string(b))
		} else {
			for _, r := range results {
				fmt.Printf("%s samples=[%d..%d) bytes=%d %s\n",
					r.Out, r.SampleStart, r.SampleEnd, r.Bytes, r.Status)
			}
		}
		return report.ExitOK, nil
	}
}
