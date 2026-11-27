// Package pipeline orchestrates the staged data flow: block recovery,
// timeline mixing and frame rendering, with content-addressed checkpoints
// so interrupted or repeated runs resume from the earliest affected stage.
package pipeline

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"avcheck/internal/cache"
	"avcheck/internal/clock"
	"avcheck/internal/fixture"
	"avcheck/internal/nettrace"
	"avcheck/internal/render"
	"avcheck/internal/timeline"
	"avcheck/internal/wav"
)

// Params are the run parameters; they feed the parameter digests.
type Params struct {
	Seed           int64
	Loss, JitterMs float64
	OutDir         string
	FPS            int
	Width, Height  int
}

// GapMark records an unrecovered hole, kept explicit so nothing shifts.
type GapMark struct {
	Track string
	Seq   uint64
	Start clock.Tick
}

// Result carries everything the demo reports on.
type Result struct {
	Mixed     []int16
	Frames    []render.Frame
	Events    []render.Event
	Duration  clock.Tick
	Stats     nettrace.Stats
	Gaps      []GapMark
	Config    timeline.Config
	CacheHits int
	Latencies []float64
}

var stages = []string{"recover", "mix", "render"}

type recoverManifest struct {
	Tracks []struct {
		Name   string `json:"name"`
		Blob   string `json:"blob"`
		Rate   int    `json:"rate"`
		Layout string `json:"layout"`
	} `json:"tracks"`
	Stats nettrace.Stats `json:"stats"`
	Gaps  []GapMark      `json:"gaps"`
}

// Run executes the pipeline. Source material and network conditions are
// regenerated deterministically from the seed, so a re-run after an
// interruption sees byte-identical inputs.
func Run(p Params) (*Result, error) {
	src := fixture.Generate()
	nets := make([]*fixture.Transmission, len(src.Tracks))
	var lat []float64
	for i, tr := range src.Tracks {
		nets[i] = fixture.Transmit(tr.Blocks, p.Seed+int64(i)*7919, p.Loss, p.JitterMs)
		lat = append(lat, nets[i].Latencies...)
	}
	chain := &cache.Chain{
		Dir:    filepath.Join(p.OutDir, "checkpoints"),
		Store:  &cache.Store{Dir: filepath.Join(p.OutDir, "cas")},
		Stages: stages,
	}
	res := &Result{Events: src.Events, Duration: src.Duration, Latencies: lat}

	srcDigest := sourceDigest(src)
	paramDigest := cache.DigestOf([]byte(fmt.Sprintf("v3|%d|%g|%g", p.Seed, p.Loss, p.JitterMs)))

	// Stage 1: recover.
	man, hit, err := recoverStage(chain, src, nets, srcDigest, paramDigest)
	if err != nil {
		return nil, err
	}
	if hit {
		res.CacheHits++
	}
	res.Stats = man.Stats
	res.Gaps = man.Gaps
	tracks, err := loadRecovered(chain, man)
	if err != nil {
		return nil, err
	}

	// Stage 2: mix.
	mixDigest := cache.DigestOf([]byte(manDigest(chain, man)))
	mixed, cfg, hit, err := mixStage(chain, tracks, src, mixDigest)
	if err != nil {
		return nil, err
	}
	if hit {
		res.CacheHits++
	}
	res.Mixed = mixed
	res.Config = cfg

	// Stage 3: render.
	frames, hit, err := renderStage(chain, p, mixed, src, mixDigest)
	if err != nil {
		return nil, err
	}
	if hit {
		res.CacheHits++
	}
	res.Frames = frames
	return res, nil
}

func sourceDigest(src *fixture.Source) string {
	var all []byte
	for _, tr := range src.Tracks {
		for _, s := range tr.PCM.Samples {
			all = binary.LittleEndian.AppendUint16(all, uint16(s))
		}
	}
	return cache.DigestOf(all)
}

func manDigest(_ *cache.Chain, man *recoverManifest) string {
	b, _ := json.Marshal(man)
	return string(b)
}

// recoverStage reorders and repairs each track's block stream, caching the
// recovered PCM per track.
func recoverStage(chain *cache.Chain, src *fixture.Source, nets []*fixture.Transmission,
	srcDigest, paramDigest string) (*recoverManifest, bool, error) {
	if ck, ok := chain.Valid("recover", "fixture", srcDigest, paramDigest); ok {
		data, err := chain.Store.Get(ck.BlobID)
		if err != nil {
			return nil, false, err
		}
		var man recoverManifest
		if err := json.Unmarshal(data, &man); err != nil {
			return nil, false, err
		}
		return &man, true, nil
	}
	man := &recoverManifest{}
	for i, tr := range src.Tracks {
		last := uint64(len(tr.Blocks) - 1)
		audioBlocks := uint64(len(tr.Blocks) - fixture.FlushBlocks)
		items, st := nettrace.Recover(nets[i].Arrived, 0, last)
		man.Stats.Received += st.Received
		man.Stats.Duplicates += st.Duplicates
		man.Stats.Lost += st.Lost
		man.Stats.Recovered += st.Recovered
		ch := tr.Layout.Channels()
		var samples []int16
		for _, it := range items {
			if it.Gap {
				if it.Seq >= audioBlocks {
					continue // lost flush block: no audio impact
				}
				man.Stats.Gaps++
				n := tr.Rate * fixture.BlockMS / 1000 * ch
				samples = append(samples, make([]int16, n)...)
				man.Gaps = append(man.Gaps, GapMark{Track: tr.Name, Seq: it.Seq,
					Start: clock.FromSamples(int64(it.Seq)*int64(tr.Rate*fixture.BlockMS/1000), tr.Rate)})
				continue
			}
			samples = append(samples, it.Block.Samples...)
		}
		pcm := &wav.PCM{Rate: tr.Rate, Layout: tr.Layout, Samples: samples}
		blob, err := chain.Store.Put(wav.EncodeWAV(pcm))
		if err != nil {
			return nil, false, err
		}
		man.Tracks = append(man.Tracks, struct {
			Name   string `json:"name"`
			Blob   string `json:"blob"`
			Rate   int    `json:"rate"`
			Layout string `json:"layout"`
		}{tr.Name, blob, tr.Rate, tr.Layout.String()})
	}
	data, _ := json.Marshal(man)
	blob, err := chain.Store.Put(data)
	if err != nil {
		return nil, false, err
	}
	ck := &cache.Checkpoint{Stage: "recover", SourceID: "fixture",
		SourceDigest: srcDigest, ParamDigest: paramDigest, PrevDigest: "-", BlobID: blob}
	if err := chain.Save(ck); err != nil {
		return nil, false, err
	}
	return man, false, nil
}

// loadRecovered turns the manifest into timeline tracks.
func loadRecovered(chain *cache.Chain, man *recoverManifest) ([]timeline.Track, error) {
	src := fixture.Generate()
	var tracks []timeline.Track
	for i, tm := range man.Tracks {
		data, err := chain.Store.Get(tm.Blob)
		if err != nil {
			return nil, err
		}
		pcm, err := wav.DecodeWAV(data)
		if err != nil {
			return nil, err
		}
		tracks = append(tracks, timeline.Track{
			Name:  tm.Name,
			Color: src.Tracks[i].Color,
			Segments: []timeline.Segment{
				{Start: 0, PCM: pcm},
			},
		})
	}
	return tracks, nil
}

// mixStage resolves the timeline config and mixes to stereo 48kHz.
func mixStage(chain *cache.Chain, tracks []timeline.Track, src *fixture.Source,
	srcDigest string) ([]int16, timeline.Config, bool, error) {
	paramDigest := cache.DigestOf([]byte("mix-v1"))
	if ck, ok := chain.Valid("mix", "timeline", srcDigest, paramDigest); ok {
		data, err := chain.Store.Get(ck.BlobID)
		if err != nil {
			return nil, timeline.Config{}, false, err
		}
		pcm, err := wav.DecodeWAV(data)
		if err != nil {
			return nil, timeline.Config{}, false, err
		}
		cfg, err := timeline.ResolveConfig(tracks)
		return pcm.Samples, cfg, true, err
	}
	cfg, err := timeline.ResolveConfig(tracks)
	if err != nil {
		return nil, cfg, false, err
	}
	mixed := timeline.Mix(tracks, src.Duration)
	pcm := &wav.PCM{Rate: timeline.Rate, Layout: wav.Stereo, Samples: mixed}
	blob, err := chain.Store.Put(wav.EncodeWAV(pcm))
	if err != nil {
		return nil, cfg, false, err
	}
	prev, err := chain.Load("recover")
	if err != nil {
		return nil, cfg, false, err
	}
	ck := &cache.Checkpoint{Stage: "mix", SourceID: "timeline",
		SourceDigest: srcDigest, ParamDigest: paramDigest,
		PrevDigest: prev.Digest, BlobID: blob}
	if err := chain.Save(ck); err != nil {
		return nil, cfg, false, err
	}
	return mixed, cfg, false, nil
}

// renderStage draws the frame sequence (or replays the cached manifest).
func renderStage(chain *cache.Chain, p Params, mixed []int16, src *fixture.Source,
	srcDigest string) ([]render.Frame, bool, error) {
	paramDigest := cache.DigestOf([]byte(
		fmt.Sprintf("render-v1|%d|%d|%d", p.FPS, p.Width, p.Height)))
	if ck, ok := chain.Valid("render", "frames", srcDigest, paramDigest); ok {
		data, err := chain.Store.Get(ck.BlobID)
		if err != nil {
			return nil, false, err
		}
		return parseManifest(string(data)), true, nil
	}
	spec := render.Spec{Width: p.Width, Height: p.Height, FPS: p.FPS, Duration: src.Duration}
	frames, err := render.Render(filepath.Join(p.OutDir, "frames"), spec, mixed, src.Events)
	if err != nil {
		return nil, false, err
	}
	var sb strings.Builder
	for _, f := range frames {
		fmt.Fprintf(&sb, "%s %d\n", f.SHA256, f.Index)
	}
	blob, err := chain.Store.Put([]byte(sb.String()))
	if err != nil {
		return nil, false, err
	}
	prev, err := chain.Load("mix")
	if err != nil {
		return nil, false, err
	}
	ck := &cache.Checkpoint{Stage: "render", SourceID: "frames",
		SourceDigest: srcDigest, ParamDigest: paramDigest,
		PrevDigest: prev.Digest, BlobID: blob}
	if err := chain.Save(ck); err != nil {
		return nil, false, err
	}
	return frames, false, nil
}

func parseManifest(s string) []render.Frame {
	var frames []render.Frame
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		var hash string
		var idx int
		if _, err := fmt.Sscanf(line, "%s %d", &hash, &idx); err == nil {
			frames = append(frames, render.Frame{Index: idx, SHA256: hash})
		}
	}
	return frames
}
