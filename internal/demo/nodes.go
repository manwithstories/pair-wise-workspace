package demo

import (
	"os"
	"path/filepath"

	"mediacore/internal/clock"
	"mediacore/internal/fixture"
	"mediacore/internal/manifest"
	"mediacore/internal/media"
	"mediacore/internal/pipeline"
	"mediacore/internal/provenance"
	"mediacore/internal/segment"
	"mediacore/internal/timeline"
)

const (
	silenceWindow  = 480 // 20ms at 24kHz
	silenceThresh  = 100
	fingerprintWin = 4096 // int16 samples = 8192 payload bytes
)

// Nodes builds the demo pipeline graph.
func Nodes(p Params, fix *fixture.Result) []pipeline.Node {
	return []pipeline.Node{
		parseHLSNode(p, fix), parseDASHNode(p), timelineNode(),
		mediaIndexNode(p), cutNode(p), silenceNode(p),
		fingerprintNode(p), bitrateNode(), rtpNode(p), reportNode(),
	}
}

func prov(node, timeBase string, r [2]int) func(*pipeline.Context, any) provenance.Record {
	return func(ctx *pipeline.Context, _ any) provenance.Record {
		return provenance.Record{
			Output:               node,
			SourceManifestDigest: ctx.InputDigest,
			SegmentRange:         r,
			TimeBase:             timeBase,
			AlgoVersion:          Version,
			ParamsDigest:         ctx.ParamsDigest,
			ErrorChain:           append([]string{}, ctx.ErrorChain...),
		}
	}
}

func parseHLSNode(p Params, fix *fixture.Result) pipeline.Node {
	return pipeline.Node{
		Name: "parse-hls", Version: Version,
		New: func() any { return &HlsResult{} },
		Run: func(ctx *pipeline.Context) (any, error) {
			res := &HlsResult{}
			files := []struct {
				name string
				meta manifest.TrackMeta
			}{
				{"video.m3u8", fix.VideoMeta},
				{"audio.m3u8", fix.AudioMeta},
				{"replay.m3u8", fix.ReplayMeta},
			}
			pl := &manifest.Playlist{Format: "hls"}
			for _, f := range files {
				data, err := os.ReadFile(filepath.Join(p.Dir, f.name))
				if err != nil {
					return nil, clock.Wrap(clock.CodeIO, "parse-hls", err)
				}
				tr, err := manifest.ParseHLS(bytesReader(data), f.meta)
				if err != nil {
					return nil, err
				}
				pl.Tracks = append(pl.Tracks, tr)
			}
			res.Tracks = pl.Tracks
			res.Digest = pl.Digest()
			return res, nil
		},
		Prov: prov("parse-hls", "1/90000", [2]int{0, fixture.VideoSegs - 1}),
	}
}

func parseDASHNode(p Params) pipeline.Node {
	return pipeline.Node{
		Name: "parse-dash", Version: Version,
		New: func() any { return &DashResult{} },
		Run: func(ctx *pipeline.Context) (any, error) {
			f, err := os.Open(filepath.Join(p.Dir, "manifest.mpd"))
			if err != nil {
				return nil, clock.Wrap(clock.CodeIO, "parse-dash", err)
			}
			defer f.Close()
			pl, err := manifest.ParseDASH(f)
			if err != nil {
				return nil, err
			}
			return &DashResult{Tracks: pl.Tracks, Digest: pl.Digest()}, nil
		},
		Prov: prov("parse-dash", "1/90000", [2]int{0, fixture.VideoSegs - 1}),
	}
}

func timelineNode() pipeline.Node {
	return pipeline.Node{
		Name: "timeline", Deps: []string{"parse-hls", "parse-dash"}, Version: Version,
		New: func() any { return &TimelineResult{} },
		Run: func(ctx *pipeline.Context) (any, error) {
			hls := ctx.Get("parse-hls").(*HlsResult)
			dash := ctx.Get("parse-dash").(*DashResult)
			hlsTL, err := timeline.Build(&manifest.Playlist{Format: "hls", Tracks: hls.Tracks})
			if err != nil {
				return nil, err
			}
			dashTL, err := timeline.Build(&manifest.Playlist{Format: "dash", Tracks: dash.Tracks})
			if err != nil {
				return nil, err
			}
			return &TimelineResult{
				AlignmentMs: timeline.AlignmentMs(hlsTL, dashTL),
				Tracks:      hlsTL.Tracks,
			}, nil
		},
		Prov: prov("timeline", "1/90000", [2]int{0, fixture.VideoSegs - 1}),
	}
}

func mediaIndexNode(p Params) pipeline.Node {
	return pipeline.Node{
		Name: "media-index", Deps: []string{"parse-hls"}, Version: Version,
		New: func() any { return &MediaIndexResult{} },
		Run: func(ctx *pipeline.Context) (any, error) {
			hls := ctx.Get("parse-hls").(*HlsResult)
			res := &MediaIndexResult{Tracks: map[string][]SegmentIndex{}}
			for _, tr := range hls.Tracks {
				var idx []SegmentIndex
				for _, seg := range tr.Segments {
					if seg.Gap {
						idx = append(idx, SegmentIndex{Gap: true})
						continue
					}
					f, err := os.Open(filepath.Join(p.Dir, seg.URI))
					if err != nil {
						return nil, clock.Wrap(clock.CodeIO, "media-index", err)
					}
					ms, err := media.ReadSegment(f, tr.ID, seg.Index, false)
					f.Close()
					if err != nil {
						if clock.CodeOf(err) == clock.CodeTruncatedSegment {
							res.Issues = append(res.Issues, err.Error())
							idx = append(idx, SegmentIndex{Truncated: true})
							continue
						}
						return nil, err
					}
					idx = append(idx, SegmentIndex{DecodeTime: ms.DecodeTime, Samples: ms.Samples})
				}
				res.Tracks[tr.ID] = idx
			}
			return res, nil
		},
		Prov: prov("media-index", "1/90000", [2]int{0, fixture.VideoSegs - 1}),
	}
}

func cutNode(p Params) pipeline.Node {
	return pipeline.Node{
		Name: "cut", Deps: []string{"timeline", "media-index"}, Version: Version,
		New: func() any { return &CutResult{} },
		Run: func(ctx *pipeline.Context) (any, error) {
			tl := ctx.Get("timeline").(*TimelineResult)
			mi := ctx.Get("media-index").(*MediaIndexResult)
			var tt *timeline.TrackTimeline
			for i := range tl.Tracks {
				if tl.Tracks[i].Meta.ID == "v1" {
					tt = &tl.Tracks[i]
				}
			}
			if tt == nil {
				return nil, clock.At(clock.CodeDependencyMissing, "cut", "v1", -1, "no v1 timeline")
			}
			idx := mediaSegments(mi.Tracks["v1"])
			start, _ := clock.New(int64(p.CutStart*1000), 1000)
			end, _ := clock.New(int64(p.CutEnd*1000), 1000)
			res, err := segment.Cut(tt, idx, segment.Request{TrackID: "v1", Start: start, End: end})
			if err != nil {
				return nil, err
			}
			out := &CutResult{
				TrackID:          "v1",
				StartRequested:   res.Start.Requested,
				StartActual:      res.Start.Actual,
				EndRequested:     res.End.Requested,
				EndActual:        res.End.Actual,
				DeviationStartMs: res.Start.DeviationMs(),
				DeviationEndMs:   res.End.DeviationMs(),
			}
			if len(res.Segments) > 0 {
				out.SegmentRange = [2]int{res.Segments[0].Index, res.Segments[len(res.Segments)-1].Index}
			}
			return out, nil
		},
		Prov: prov("cut", "1/90000", [2]int{1, 2}),
	}
}

func mediaSegments(idx []SegmentIndex) []*media.Segment {
	out := make([]*media.Segment, len(idx))
	for i, si := range idx {
		if si.Gap || si.Truncated {
			continue
		}
		out[i] = &media.Segment{DecodeTime: si.DecodeTime, Samples: si.Samples}
	}
	return out
}
