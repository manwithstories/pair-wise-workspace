// Package pipeline coordinates batch processing: adapters → inspect →
// index → repair → remux → manifest, with per-segment checkpoints so
// repeated runs over the same input are identical and a damaged
// checkpoint only forces recomputation from the nearest valid boundary.
package pipeline

import (
	"sync"

	"media-check/internal/adapter"
	"media-check/internal/adapter/audio"
	"media-check/internal/adapter/frame"
	mp4adapter "media-check/internal/adapter/mp4"
	"media-check/internal/core"
	"media-check/internal/index"
	"media-check/internal/manifest"
	"media-check/internal/model"
)

// Asset is one delivered unit of input.
type Asset struct {
	Name          string
	Kind          string // "frames" | "mp4"
	Packets       [][]byte
	MP4Data       []byte
	Audio         []audio.Meta
	StartSeq      uint32
	SegmentFrames int
	TSDelta       uint32
}

// AssetResult is the full processing outcome of one asset.
type AssetResult struct {
	Name           string
	Segments       []manifest.SegmentEntry
	Faults         model.Faults
	AudioDecisions []audio.Decision
	ColorDecisions []core.ColorDecision
	CorruptRanges  int
	KeyHits        int
	KeyTotal       int
	RoundtripOK    int
	RoundtripTotal int
}

// Checkpoint caches one segment's result at the segment boundary.
type Checkpoint struct {
	InputHash    [32]byte
	Entry        manifest.SegmentEntry
	RoundtripOK  bool
	RoundtripSet bool
}

// Store is an in-memory checkpoint store keyed by asset+segment.
type Store struct {
	mu sync.Mutex
	m  map[string]map[int]Checkpoint
}

func NewStore() *Store { return &Store{m: map[string]map[int]Checkpoint{}} }

func (s *Store) load(asset string, seg int) (Checkpoint, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, ok := s.m[asset][seg]
	return cp, ok
}

func (s *Store) save(asset string, seg int, cp Checkpoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m[asset] == nil {
		s.m[asset] = map[int]Checkpoint{}
	}
	s.m[asset][seg] = cp
}

// Pipeline runs assets through the kernel with registered adapters.
type Pipeline struct {
	Reg   *adapter.Registry
	Store *Store
}

func NewPipeline(reg *adapter.Registry, store *Store) *Pipeline {
	return &Pipeline{Reg: reg, Store: store}
}

// RunAsset processes one asset. Unknown adapter versions reject only
// this asset; the rest of the batch is unaffected.
func (p *Pipeline) RunAsset(a Asset) (res AssetResult) {
	res.Name = a.Name
	switch a.Kind {
	case "frames":
		p.runFrames(a, &res)
	case "mp4":
		p.runMP4(a, &res)
	default:
		res.Faults = append(res.Faults, model.Fault{
			Code: model.FaultUnknownAdapter, Segment: -1,
			Detail: "unknown asset kind " + a.Kind,
		})
	}
	return res
}

// segmentFrames groups frames by delivery sequence so boundaries stay
// stable even when corrupt frames were dropped during ingest.
func segmentFrames(frames []model.Frame, a Asset) [][]model.Frame {
	n := 0
	for _, f := range frames {
		if s := int((f.Seq-a.StartSeq)/uint32(a.SegmentFrames)) + 1; s > n {
			n = s
		}
	}
	out := make([][]model.Frame, n)
	for _, f := range frames {
		s := int((f.Seq - a.StartSeq) / uint32(a.SegmentFrames))
		out[s] = append(out[s], f)
	}
	return out
}

func (p *Pipeline) runFrames(a Asset, res *AssetResult) {
	ad, fault := p.Reg.Get(frame.AdapterID, frame.Version)
	if fault != nil {
		res.Faults = append(res.Faults, *fault)
		return
	}
	ing := ad.(frame.Adapter).Ingest(a.Packets, func(seq uint32) int {
		return int((seq - a.StartSeq) / uint32(a.SegmentFrames))
	})
	res.Faults = append(res.Faults, ing.Faults...)

	segs := segmentFrames(ing.Frames, a)
	segRanges := make([]model.TimeRange, len(segs))
	for i, sf := range segs {
		if len(sf) > 0 {
			segRanges[i] = model.TimeRange{Start: sf[0].Time, End: sf[len(sf)-1].Time + int64(a.TSDelta)}
		}
	}

	rep := core.Inspect(ing.Frames, a.SegmentFrames)
	res.Faults = append(res.Faults, rep.Faults...)
	res.ColorDecisions = rep.ColorDecisions

	idx := index.Build(ing.Frames)
	hits, total := index.Verify(idx, ing.Frames)
	res.KeyHits, res.KeyTotal = hits, total

	if len(a.Audio) > 0 {
		aAd, aFault := p.Reg.Get(audio.AdapterID, audio.Version)
		if aFault != nil {
			res.Faults = append(res.Faults, *aFault)
		} else {
			ar := aAd.(audio.Adapter).Normalize(a.Audio)
			res.Faults = append(res.Faults, ar.Faults...)
			res.AudioDecisions = ar.Decisions
		}
	}

	plan := core.BuildPlan(segRanges, res.Faults)
	res.CorruptRanges = plan.CorruptRanges

	mp4Ad, mp4Fault := p.Reg.Get(mp4adapter.AdapterID, mp4adapter.Version)
	if mp4Fault != nil {
		res.Faults = append(res.Faults, *mp4Fault)
		return
	}
	res.Segments = make([]manifest.SegmentEntry, len(segs))
	recompute := false
	for i, sf := range segs {
		entry := manifest.SegmentEntry{Index: i, SrcRange: segRanges[i], Status: manifest.StatusOK}
		if len(sf) == 0 {
			entry.Status = manifest.StatusCorrupt
			entry.Reason = "no valid frames in segment"
			res.Segments[i] = entry
			continue
		}
		entry.SrcHash = manifest.HashFrames(sf)
		if plan.Segments[i].Kind == core.SegmentCorrupt {
			entry.Status = manifest.StatusCorrupt
			entry.Reason = joinReasons(plan.Segments[i].Reasons)
			res.Segments[i] = entry
			continue
		}
		inputHash := manifest.HashFrames(sf)
		if !recompute {
			if cp, ok := p.Store.load(a.Name, i); ok && cp.InputHash == inputHash {
				res.Segments[i] = cp.Entry
				if cp.RoundtripSet {
					res.RoundtripTotal++
					if cp.RoundtripOK {
						res.RoundtripOK++
					}
				}
				continue
			}
			recompute = true // nearest invalid boundary: recompute from here
		}
		rm, err := core.RemuxSegment(mp4Ad.(mp4adapter.Adapter), sf, i, a.TSDelta)
		res.RoundtripTotal++
		if err != nil {
			entry.Status = manifest.StatusCorrupt
			entry.Reason = err.Error()
		} else {
			entry.DstHash = rm.DstHash // hash of frames rebuilt from output
			entry.DstRange = rm.Range
			if rm.RoundtripOK {
				res.RoundtripOK++
			}
		}
		res.Segments[i] = entry
		p.Store.save(a.Name, i, Checkpoint{
			InputHash: inputHash, Entry: entry,
			RoundtripOK: err == nil && rm.RoundtripOK, RoundtripSet: true,
		})
	}
}

func (p *Pipeline) runMP4(a Asset, res *AssetResult) {
	ad, fault := p.Reg.Get(mp4adapter.AdapterID, mp4adapter.Version)
	if fault != nil {
		res.Faults = append(res.Faults, *fault)
		return
	}
	m := ad.(mp4adapter.Adapter)
	parsed := m.Parse(a.MP4Data, 0, model.TimeRange{})
	res.Faults = append(res.Faults, parsed.Faults...)
	entry := manifest.SegmentEntry{Index: 0, Status: manifest.StatusOK}
	if !parsed.Complete {
		entry.Status = manifest.StatusCorrupt
		for _, f := range parsed.Faults {
			entry.Reason += string(f.Code) + " "
		}
		res.CorruptRanges = 1
		res.Segments = []manifest.SegmentEntry{entry}
		return
	}
	samples, err := parsed.Table.Samples(a.MP4Data)
	if err != nil {
		entry.Status = manifest.StatusCorrupt
		entry.Reason = err.Error()
		res.CorruptRanges = 1
		res.Segments = []manifest.SegmentEntry{entry}
		return
	}
	entry.SrcHash = manifest.HashBytes(concat(samples))
	out, err := m.Marshal(samples, parsed.Table.KeySamples, parsed.Table.Timescale, parsed.Table.Delta)
	if err != nil {
		entry.Status = manifest.StatusCorrupt
		entry.Reason = err.Error()
	} else if re := m.Parse(out, 0, model.TimeRange{}); !re.Complete {
		entry.Status = manifest.StatusCorrupt
		entry.Reason = "re-parse of remuxed output failed"
	} else if got, err := re.Table.Samples(out); err != nil {
		entry.Status = manifest.StatusCorrupt
		entry.Reason = err.Error()
	} else {
		entry.DstHash = manifest.HashBytes(concat(got))
		res.RoundtripTotal++
		if entry.DstHash == entry.SrcHash {
			res.RoundtripOK++
		}
	}
	res.KeyTotal = len(parsed.Table.KeySamples)
	res.KeyHits = res.KeyTotal
	res.Segments = []manifest.SegmentEntry{entry}
}

func concat(bs [][]byte) []byte {
	var n int
	for _, b := range bs {
		n += len(b)
	}
	out := make([]byte, 0, n)
	for _, b := range bs {
		out = append(out, b...)
	}
	return out
}

func joinReasons(rs []string) string {
	s := ""
	for i, r := range rs {
		if i > 0 {
			s += "; "
		}
		s += r
	}
	return s
}
