package align

import (
	"fmt"
	"sort"

	"github.com/relayteam/relay/internal/media"
	"github.com/relayteam/relay/internal/segment"
)

// MaxBoundaryTicks is the shared-cut tolerance from the specification.
const MaxBoundaryTicks media.Tick = 1

// Span is one representation's coverage of a shared segment index.
type Span struct {
	StreamKey      string `json:"stream_key"`
	Track          string `json:"track"`
	Rendition      string `json:"rendition"`
	SegmentIndex   int    `json:"segment_index"`
	Start90k       int64  `json:"start90k"`
	End90k         int64  `json:"end90k"`
	ActualFirstPTS int64  `json:"actual_first_pts90k"`
	ActualLastEnd  int64  `json:"actual_last_end90k"`
	BoundaryDelta  int64  `json:"boundary_delta_ticks"`
	Records        int    `json:"records"`
	PayloadBytes   int64  `json:"payload_bytes"`
}

// Representation contains aligned spans for one track/bitrate layer.
type Representation struct {
	Key            string  `json:"key"`
	Track          string  `json:"track"`
	Rendition      string  `json:"rendition"`
	Spans          []*Span `json:"spans"`
	PrecisionTicks int64   `json:"precision_ticks"`
}

// Report is the representation-independent alignment result.
type Report struct {
	Grid            int64                      `json:"grid90k"`
	Keys            []string                   `json:"keys"`
	Representations map[string]*Representation `json:"representations"`
	SegmentCount    int                        `json:"segment_count"`
	Duration90k     int64                      `json:"duration90k"`
	PrecisionTicks  int64                      `json:"precision_ticks"`
}

// Aligner incrementally groups fragments without retaining their payloads.
type Aligner struct {
	grid   int64
	spans  map[string]map[int]*Span
	first  map[string]media.Meta
	latest map[string]media.Meta
}

// NewAligner creates an aligner on the same shared grid as segment.Cutter.
func NewAligner(gridTicks int64) *Aligner {
	if gridTicks <= 0 {
		gridTicks = segment.DefaultSegmentTicks
	}
	return &Aligner{
		grid:   gridTicks,
		spans:  make(map[string]map[int]*Span),
		first:  make(map[string]media.Meta),
		latest: make(map[string]media.Meta),
	}
}

func keyOf(f segment.Fragment) string {
	return f.Record.Track + "/" + f.Record.Rendition
}

// Add incorporates one cut-assigned fragment. It enforces the one-tick
// boundary limit and returns a unified error at the offending source record.
func (a *Aligner) Add(f segment.Fragment, payloadBytes int) error {
	key := keyOf(f)
	byIndex, ok := a.spans[key]
	if !ok {
		byIndex = make(map[int]*Span)
		a.spans[key] = byIndex
	}
	s, ok := byIndex[f.SegmentIndex]
	if !ok {
		s = &Span{
			StreamKey:      key,
			Track:          f.Record.Track,
			Rendition:      f.Record.Rendition,
			SegmentIndex:   f.SegmentIndex,
			Start90k:       f.CutStart90k,
			End90k:         f.CutEnd90k,
			ActualFirstPTS: f.Record.PTS90k,
			ActualLastEnd:  f.Record.PTS90k + f.Record.Dur90k,
		}
		byIndex[f.SegmentIndex] = s
		a.first[key] = f.Record.Meta(f.Record.RecordNo, f.Record.LineNo)
	}
	s.Records++
	s.PayloadBytes += int64(payloadBytes)
	meta := f.Record.Meta(f.Record.RecordNo, f.Record.LineNo)
	a.latest[key] = meta
	if f.Record.PTS90k < s.ActualFirstPTS {
		s.ActualFirstPTS = f.Record.PTS90k
	}
	if end := f.Record.PTS90k + f.Record.Dur90k; end > s.ActualLastEnd {
		s.ActualLastEnd = end
	}
	startDelta := abs64(s.ActualFirstPTS - s.Start90k)
	// End delta is recomputed at finalization because a partial span is not
	// expected to reach its shared end yet.

	// Start boundaries can be rejected immediately. End boundaries are only
	// final after a complete span has accumulated all of its records.
	if media.Tick(startDelta) > MaxBoundaryTicks {
		return media.E(media.ErrBoundaryMismatch, "align.representations", meta, fmt.Errorf(
			"representation %s segment %d starts %d ticks from shared cut (limit %d)",
			key, f.SegmentIndex, startDelta, MaxBoundaryTicks))
	}
	return nil
}

// Report finalizes and cross-checks every representation's coverage.
func (a *Aligner) Report() (*Report, error) {
	r := &Report{Grid: a.grid, Representations: make(map[string]*Representation)}
	for key, byIndex := range a.spans {
		indexes := make([]int, 0, len(byIndex))
		for idx := range byIndex {
			indexes = append(indexes, idx)
		}
		sort.Ints(indexes)
		rep := &Representation{Key: key}
		if s := byIndex[indexes[0]]; s != nil {
			rep.Track, rep.Rendition = s.Track, s.Rendition
		}
		for _, idx := range indexes {
			s := byIndex[idx]
			startDelta := abs64(s.ActualFirstPTS - s.Start90k)
			endDelta := abs64(s.ActualLastEnd - s.End90k)
			s.BoundaryDelta = startDelta
			if endDelta > s.BoundaryDelta {
				s.BoundaryDelta = endDelta
			}
			rep.Spans = append(rep.Spans, s)
			if s.BoundaryDelta > rep.PrecisionTicks {
				rep.PrecisionTicks = s.BoundaryDelta
			}
			if media.Tick(s.BoundaryDelta) > MaxBoundaryTicks {
				meta := a.latest[key]
				return nil, media.E(media.ErrBoundaryMismatch, "align.representations", meta, fmt.Errorf(
					"representation %s segment %d boundary deviates %d ticks (limit %d)",
					key, idx, s.BoundaryDelta, MaxBoundaryTicks))
			}
		}
		r.Keys = append(r.Keys, key)
		r.Representations[key] = rep
		if rep.PrecisionTicks > r.PrecisionTicks {
			r.PrecisionTicks = rep.PrecisionTicks
		}
	}
	sort.Strings(r.Keys)
	if err := r.checkCoverage(); err != nil {
		return nil, err
	}
	if len(r.Keys) > 0 {
		first := r.Representations[r.Keys[0]]
		r.SegmentCount = len(first.Spans)
		if r.SegmentCount > 0 {
			r.Duration90k = first.Spans[len(first.Spans)-1].End90k - first.Spans[0].Start90k
		}
	}
	return r, nil
}

func (r *Report) checkCoverage() error {
	if len(r.Keys) == 0 {
		return nil
	}
	base := r.Representations[r.Keys[0]]
	wantCount := len(base.Spans)
	wantStart := base.Spans[0].Start90k
	wantEnd := base.Spans[wantCount-1].End90k
	for _, key := range r.Keys[1:] {
		rep := r.Representations[key]
		if len(rep.Spans) != wantCount {
			return coverageError(key, rep, fmt.Sprintf("segment count %d != %d", len(rep.Spans), wantCount))
		}
		for i, s := range rep.Spans {
			bs := base.Spans[i]
			if s.Start90k != bs.Start90k || s.End90k != bs.End90k {
				return coverageError(key, rep, fmt.Sprintf("segment %d range %d-%d != %d-%d",
					i, s.Start90k, s.End90k, bs.Start90k, bs.End90k))
			}
		}
		gotEnd := rep.Spans[len(rep.Spans)-1].End90k
		if rep.Spans[0].Start90k != wantStart || gotEnd != wantEnd {
			return coverageError(key, rep, "duration coverage differs")
		}
	}
	return nil
}

func coverageError(key string, rep *Representation, detail string) error {
	meta := media.Meta{Track: rep.Track, Rendition: rep.Rendition, RecordNo: -1}
	return media.E(media.ErrDurationMismatch, "align.representations", meta, fmt.Errorf("%s: %s", key, detail))
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// AnnotateError moves a unified error through alignment.
func AnnotateError(err error) error {
	return media.Annotate(err, "align.representations")
}
