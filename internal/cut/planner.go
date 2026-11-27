// Package cut turns the unified timeline into concrete cut boundaries that are
// aligned to both video frame and audio sample edges. Boundaries are produced
// monotonically so a timestamp wraparound or PTS reset can never move a cut
// backwards, and every boundary is snap-aligned before being emitted.
package cut

import (
	"sort"

	"mediadelivery/internal/clock"
)

// TrackSpec describes the alignment grid of one track.
type TrackSpec struct {
	// FrameRateNum/Den define the video frame rate (zero for audio-only tracks).
	FrameRateNum int
	FrameRateDen int
	// SampleRate is the audio sample rate (zero for video-only tracks).
	SampleRate int
}

// Request is an unaligned desired cut position on the 90 kHz shared clock.
type Request struct {
	// Label names the cut (e.g. a segment or silence boundary).
	Label string
	// At90k is the requested position.
	At90k int64
}

// Boundary is a final, aligned cut position.
type Boundary struct {
	Label     string `json:"label"`
	// Requested90k is the original request before alignment.
	Requested90k int64 `json:"requested_90k"`
	// Video90k / Audio90k are the per-track aligned positions.
	Video90k int64 `json:"video_90k"`
	Audio90k int64 `json:"audio_90k"`
	// Cut90k is the operative position (the earlier of the two aligned edges so
	// no samples/frames are dropped).
	Cut90k int64 `json:"cut_90k"`
}

// Planner is stateful: it tracks the last emitted cut and rejects any move
// backwards after timeline normalisation.
type Planner struct {
	spec    TrackSpec
	lastCut int64
	started bool
}

// NewPlanner builds a Planner for the given track alignment grid.
func NewPlanner(spec TrackSpec) *Planner {
	return &Planner{spec: spec}
}

// Plan aligns and orders the requests. Duplicate and out-of-order (after
// monotonic clamping) positions are collapsed deterministically. The input is
// treated as an unordered set of desired positions; for stream-order handling
// under PTS resets use Advance.
func (p *Planner) Plan(requests []Request) ([]Boundary, error) {
	// Stable sort by requested position while keeping label order on ties.
	sorted := make([]Request, len(requests))
	copy(sorted, requests)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].At90k < sorted[j].At90k
	})

	out := make([]Boundary, 0, len(sorted))
	seen := map[int64]bool{}
	for _, req := range sorted {
		b := p.align(req)
		if p.started && b.Cut90k < p.lastCut {
			b.Cut90k = p.lastCut
			b.Video90k = max64(b.Video90k, p.lastCut)
			b.Audio90k = max64(b.Audio90k, p.lastCut)
		}
		if seen[b.Cut90k] {
			continue
		}
		seen[b.Cut90k] = true
		out = append(out, b)
		p.lastCut = b.Cut90k
		p.started = true
	}
	return out, nil
}

// Advance processes one request in stream-arrival order. This is the path used
// while walking a live timeline: a PTS reset or wraparound that produces a
// smaller position is clamped so the emitted cut never moves backwards.
func (p *Planner) Advance(req Request) Boundary {
	b := p.align(req)
	if p.started && b.Cut90k < p.lastCut {
		b.Cut90k = p.lastCut
		b.Video90k = max64(b.Video90k, p.lastCut)
		b.Audio90k = max64(b.Audio90k, p.lastCut)
	}
	p.lastCut = b.Cut90k
	p.started = true
	return b
}

// align snaps one request to the video frame and audio sample grids.
func (p *Planner) align(req Request) Boundary {
	b := Boundary{Label: req.Label, Requested90k: req.At90k}
	pos := req.At90k
	if pos < 0 {
		pos = 0
	}
	if p.spec.FrameRateNum > 0 && p.spec.FrameRateDen > 0 {
		fr := clock.Rational{Num: p.spec.FrameRateNum, Den: p.spec.FrameRateDen}
		b.Video90k = clock.AlignVideo(pos, fr)
	} else {
		b.Video90k = pos
	}
	if p.spec.SampleRate > 0 {
		b.Audio90k = clock.AlignAudio(pos, p.spec.SampleRate)
	} else {
		b.Audio90k = pos
	}
	// Use the earlier aligned edge so the cut never drops content.
	b.Cut90k = min64(b.Video90k, b.Audio90k)
	return b
}

// LastCut returns the most recent emitted cut position.
func (p *Planner) LastCut() int64 { return p.lastCut }

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
