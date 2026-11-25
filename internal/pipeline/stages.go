package pipeline

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"fieldcut/internal/clock"
	"fieldcut/internal/cut"
	"fieldcut/internal/events"
	"fieldcut/internal/fingerprint"
	"fieldcut/internal/graph"
	"fieldcut/internal/ingest"
	"fieldcut/internal/recover"
)

func writeInputs(outDir string, b *Bundle) error {
	dir := filepath.Join(outDir, "input")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files := map[string][]byte{
		"director.mp4":       b.DirectorBytes,
		"stage.mp4":          b.StageBytes,
		"audio48-stereo.pcm": b.Audio48Stereo,
		"audio44-mono.pcm":   b.Audio44Mono,
		"audio48-51.pcm":     b.Audio51,
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := recover.AtomicWriteFile(filepath.Join(dir, name), files[name]); err != nil {
			return err
		}
	}
	return nil
}

func buildGraph() (*graph.Plan, []string, error) {
	p, err := graph.NewPlan(
		graph.Node{ID: "generate", Kind: "fixture"},
		graph.Node{ID: "ingest", Kind: "mp4+rtp", DependsOn: []string{"generate"}},
		graph.Node{ID: "align", Kind: "events", DependsOn: []string{"ingest"}},
		graph.Node{ID: "graph", Kind: "topology", DependsOn: []string{"align"}},
		graph.Node{ID: "audio", Kind: "pcm", DependsOn: []string{"ingest"}},
		graph.Node{ID: "cut", Kind: "boundaries", DependsOn: []string{"align", "audio"}},
		graph.Node{ID: "fingerprint", Kind: "dedupe", DependsOn: []string{"cut"}},
		graph.Node{ID: "package", Kind: "atomic-output", DependsOn: []string{"cut", "fingerprint"}},
		graph.Node{ID: "manifest", Kind: "manifest", DependsOn: []string{"package", "graph"}},
	)
	if err != nil {
		return nil, nil, err
	}
	order, err := p.TopoSort()
	return p, order, err
}

func fmtJoin(xs []string) string { return strings.Join(xs, ">") }

func fmtFloat(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }

func cutCandidates(evs []events.Event, silence []clock.Interval, video *ingest.MP4Track) []cut.Candidate {
	var out []cut.Candidate
	for _, ev := range evs {
		if ev.Kind == events.KindChapter {
			out = append(out, cut.Candidate{Time: ev.Interval.Start, Kind: cut.CandidateChapter, Note: ev.Label})
		}
	}
	for _, iv := range silence {
		out = append(out, cut.Candidate{Time: iv.Start + iv.Duration()/2, Kind: cut.CandidateSilence})
	}
	if video != nil {
		for _, s := range video.Samples {
			if s.Sync {
				out = append(out, cut.Candidate{Time: s.PTS, Kind: cut.CandidateRandomAccess})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out
}

func samplesInSegment(t *ingest.MP4Track, seg clock.Interval) []ingest.MP4Sample {
	var out []ingest.MP4Sample
	if t == nil {
		return out
	}
	for _, s := range t.Samples {
		if seg.Contains(s.PTS) {
			out = append(out, s)
		}
	}
	return out
}

func fingerprintWindows(director, stage *ingest.MP4Track, segs []clock.Interval) []fingerprint.Window {
	var windows []fingerprint.Window
	for trackIndex, tr := range []*ingest.MP4Track{director, stage} {
		if tr == nil {
			continue
		}
		prefix := "director"
		if trackIndex == 1 {
			prefix = "stage"
		}
		for i, seg := range segs {
			ss := samplesInSegment(tr, seg)
			datas := make([][]byte, 0, len(ss))
			for _, s := range ss {
				datas = append(datas, s.Data)
			}
			windows = append(windows, fingerprint.Window{
				ID: prefix + "-seg-" + strconv.Itoa(i), TrackID: tr.TrackID, Samples: datas,
			})
		}
	}
	return windows
}
