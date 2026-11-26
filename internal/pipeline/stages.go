package pipeline

import (
	"bytes"
	"time"

	"audiorestore/internal/fingerprint"
	"audiorestore/internal/model"
	"audiorestore/internal/recover"
	"audiorestore/internal/segment"
	"audiorestore/internal/silence"
	"audiorestore/internal/timeline"
	"audiorestore/internal/wav"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// processCarriers parses each carrier, checks blocks and runs parity
// recovery. Timed block processing feeds the pipeline P95 metric.
func (r *Run) processCarriers() ([]*timeline.Track, []*TrackResult, error) {
	tracks := make([]*timeline.Track, 0, len(r.carriers))
	results := make([]*TrackResult, 0, len(r.carriers))
	for _, c := range r.carriers {
		res := &TrackResult{}
		reader, err := wav.Open(bytesReader(c.WAV))
		if err != nil {
			return nil, nil, err
		}
		f := reader.Format()
		blocks, err := reader.ReadBlocks(r.log)
		if err != nil {
			return nil, nil, err
		}
		for i := range blocks {
			start := time.Now()
			// Timed per-block work: decode-validate every present block so
			// a malformed payload surfaces as a tracked error, and the
			// block-processing P95 reflects real CPU work.
			if _, derr := wav.DecodeBlock(blocks[i], f); derr != nil {
				r.log.Addf(model.ErrUnsupportedFormat, c.ID, blocks[i].Index,
					blocks[i].StartFrame, "block decode: %v", derr)
			}
			res.BlockTimes = append(res.BlockTimes, time.Since(start))
		}
		if c.DataPerGroup > 0 {
			groups := buildRecoverGroups(reader.Manifest(), c.DataPerGroup)
			rc := &recover.Recoverer{Groups: groups, Log: r.log, Track: c.ID}
			outcome := rc.Repair(blocks)
			res.Recovered, res.Unrecov = outcome.Recovered, outcome.Unrecovered
		} else {
			for _, b := range blocks {
				if b.Missing || b.Corrupt || b.Truncated {
					res.Unrecov = append(res.Unrecov, b.Index)
				}
			}
		}
		t := &timeline.Track{
			ID: c.ID, Rate: f.SampleRate, Channels: f.Channels,
			BitDepth: f.BitDepth, Layout: f.Layout, Color: f.Color,
			Frames: reader.TotalFrames(), StartFrame: c.StartFrame,
			DependsOn: append([]string(nil), c.DependsOn...),
		}
		res.Track, res.Blocks = t, blocks
		tracks = append(tracks, t)
		results = append(results, res)
	}
	return tracks, results, nil
}

// analyze runs cut, silence and fingerprint stages per track in graph
// dependency order, assigning shared storage references to segments.
func (r *Run) analyze(g *timeline.Graph, results map[string]*TrackResult) error {
	for _, id := range g.Order() {
		res := results[id]
		stageStart := time.Now()
		segs, err := segment.Cut(id, res.Track.Rate, res.Blocks, segment.Config{
			WindowFrames: r.cfg.WindowFrames, KeepPartial: r.cfg.KeepPartial,
		})
		if err != nil {
			return err
		}
		res.Segments = segs
		for si := range segs {
			s := &segs[si]
			samples := r.segmentSamples(res, s)
			sil := silence.Detect(id, res.Track.Rate, samples, s.StartFrame, silence.Config{
				Threshold: r.cfg.SilenceThresh, MinRunFrames: r.cfg.SilenceMinRun,
			})
			res.Silence = append(res.Silence, sil...)
			canon := fingerprint.Normalize(fingerprint.Content{
				TrackID: id, Rate: res.Track.Rate, Data: samples,
			})
			storageID, _ := r.table.Register(fingerprint.Hash(canon))
			s.StorageID = storageID
		}
		res.StageTimes = append(res.StageTimes, time.Since(stageStart))
	}
	return nil
}

// segmentSamples decodes exactly the source blocks covering a segment and
// trims to its frame window. Unrecovered blocks are absent by construction
// (segments never span gaps), so every returned channel is real audio.
func (r *Run) segmentSamples(res *TrackResult, s *segment.Segment) [][]int {
	var merged [][]int
	for _, idx := range s.SourceBlocks {
		dec, err := wav.DecodeBlock(res.Blocks[idx], blockFormat(res))
		if err != nil {
			r.log.Addf(model.ErrUnsupportedFormat, res.Track.ID, idx,
				res.Blocks[idx].StartFrame, "decode: %v", err)
			continue
		}
		if dec == nil {
			continue
		}
		if merged == nil {
			merged = make([][]int, len(dec))
			for c := range merged {
				merged[c] = dec[c]
			}
			continue
		}
		for c := range merged {
			merged[c] = append(merged[c], dec[c]...)
		}
	}
	if merged == nil {
		return nil
	}
	lo := int(s.StartFrame - firstSourceFrame(res, s))
	hi := int(s.EndFrame - firstSourceFrame(res, s))
	if lo < 0 {
		lo = 0
	}
	if hi > len(merged[0]) {
		hi = len(merged[0])
	}
	out := make([][]int, len(merged))
	for c := range out {
		out[c] = merged[c][lo:hi]
	}
	return out
}

func firstSourceFrame(res *TrackResult, s *segment.Segment) int64 {
	if len(s.SourceBlocks) == 0 {
		return s.StartFrame
	}
	return res.Blocks[s.SourceBlocks[0]].StartFrame
}

func blockFormat(res *TrackResult) wav.Format {
	t := res.Track
	return wav.Format{
		SampleRate: t.Rate, Channels: t.Channels, BitDepth: t.BitDepth,
		BlockAlign: t.Channels * int(t.BitDepth) / 8,
	}
}

func buildRecoverGroups(m *wav.Manifest, dataPerGroup int) []recover.Group {
	if m == nil {
		return nil
	}
	parity := map[int]bool{}
	for _, p := range m.ParityAt {
		parity[p] = true
	}
	dataBlocks := m.DataBlocks
	if dataBlocks == 0 {
		dataBlocks = len(m.CRC)
	}
	var groups []recover.Group
	for start, gid := 0, 0; start < dataBlocks; start, gid = start+dataPerGroup, gid+1 {
		end := start + dataPerGroup
		if end > dataBlocks {
			end = dataBlocks
		}
		pIdx := dataBlocks + gid
		// The manifest is authoritative: a parity block exists only when
		// it was actually declared and written into the carrier.
		if pIdx >= len(m.CRC) || !parity[pIdx] {
			continue
		}
		members := make([]int, 0, end-start+1)
		for i := start; i < end; i++ {
			members = append(members, i)
		}
		members = append(members, pIdx)
		groups = append(groups, recover.Group{ID: gid, Members: members})
	}
	return groups
}
