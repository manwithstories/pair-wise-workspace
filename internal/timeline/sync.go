// Package timeline aligns tracks on the shared nanosecond base, computes
// drift against the audio anchor and produces the ordered cue list used by the
// manifest.
package timeline

import (
	"fmt"
	"sort"

	"avcheck/internal/ebml"
	"avcheck/internal/media"
)

// Sample is one aligned media unit after transport repair. Recovered marks a
// packet whose payload came from XOR repair; Gap marks a unit with no payload
// at all, which must never receive a fabricated timestamp.
type Sample struct {
	Track      uint64
	Kind       media.TrackKind
	Seq        uint32
	TimeNs     int64
	DurationNs int64
	Recovered  bool
	Cue        string
	SourceNs   int64 // unadjusted source time, preserved for drift audit
}

// TrackTimeline is the ordered output of one track.
type TrackTimeline struct {
	Track   uint64
	Kind    media.TrackKind
	FirstNs int64
	LastNs  int64
	Samples []Sample
	// GapSeqs lists sequence numbers with no payload at all. They are counted
	// but never given a timestamp: the missing media time is not invented.
	GapSeqs   []uint32
	Gaps      int
	Recovered int
	Monotonic bool
}

// Result is the full cross-track alignment plus the transport counters the
// manifest reports. Counters are filled by the pipeline after repair so sync
// stays independent of the wire format.
type Result struct {
	Anchor       uint64
	Tracks       []TrackTimeline
	CueOrder     []uint64 // flush order from the dependency graph
	AVSkewP95Ns  int64
	AVSkewMaxNs  int64
	AVSkewMeanNs int64
	// ManifestDeltaNs is the largest |adjusted-source| shift applied to any
	// sample; zero means source timestamps were used verbatim.
	ManifestDeltaNs    int64
	PayloadBytes       int64
	PeakBufferBytes    int64
	RecoveredPackets   int
	UnrecoveredPackets int
	Duplicates         int
	RecoveredPct       float64
	Faults             *media.Faults
}

// Options tune the alignment.
type Options struct {
	// MaxSkewNs above which a cross-track anomaly is recorded. Zero means
	// 250 ms.
	MaxSkewNs int64
}

// Align builds per-track timelines from repaired samples keyed by track
// number. Samples for undeclared tracks are a hard error; tracks with no
// samples still appear (as empty timelines) so dependent flush order stays
// complete.
func Align(doc *ebml.Document, graph *ebml.Graph, samples map[uint64][]Sample, gaps map[uint64][]uint32, opt Options) (*Result, error) {
	if doc == nil || graph == nil {
		return nil, fmt.Errorf("timeline: nil document or graph")
	}
	if opt.MaxSkewNs <= 0 {
		opt.MaxSkewNs = 250 * 1e6
	}
	faults := doc.Faults
	anchor, err := graph.Anchor()
	if err != nil {
		return nil, err
	}
	order, err := graph.FinishOrder()
	if err != nil {
		return nil, err
	}
	for tnum := range samples {
		if _, ok := doc.TrackByNumber(tnum); !ok {
			faults.Addf(media.LevelAbort, media.CodeUnknownTrack, tnum, 0,
				"samples reference undeclared track %d", tnum)
			return nil, faults
		}
	}
	res := &Result{Anchor: anchor, CueOrder: order, Faults: faults}
	for _, tnum := range order {
		tl := TrackTimeline{Track: tnum, Kind: graph.Kind(tnum), Monotonic: true}
		if g, ok := gaps[tnum]; ok && len(g) > 0 {
			tl.GapSeqs = append([]uint32(nil), g...)
			tl.Gaps = len(g)
		}
		in := samples[tnum]
		// Monotonicity is judged on the arrival order, before stabilising:
		// sorting must not hide a source that arrived out of order.
		var prev int64 = -1
		for _, s := range in {
			if s.TimeNs < prev {
				tl.Monotonic = false
				faults.Addf(media.LevelDegraded, media.CodeNonMonotonic, tnum, int64(s.Seq),
					"track %d timestamp %d ns follows %d ns", tnum, s.TimeNs, prev)
			}
			prev = s.TimeNs
		}
		sort.SliceStable(in, func(i, j int) bool { return in[i].TimeNs < in[j].TimeNs })
		for _, s := range in {
			if s.Recovered {
				tl.Recovered++
			}
			s.Kind = tl.Kind
			if d := s.TimeNs - s.SourceNs; d > res.ManifestDeltaNs {
				res.ManifestDeltaNs = d
			} else if -d > res.ManifestDeltaNs {
				res.ManifestDeltaNs = -d
			}
			tl.Samples = append(tl.Samples, s)
		}
		if n := len(tl.Samples); n > 0 {
			tl.FirstNs = tl.Samples[0].TimeNs
			tl.LastNs = tl.Samples[n-1].TimeNs
		}
		res.Tracks = append(res.Tracks, tl)
	}
	res.computeSkew(opt)
	return res, nil
}

// computeSkew compares each non-audio track to the audio anchor by nearest
// pairing on the shared clock, reporting the 95th percentile and extremes.
func (r *Result) computeSkew(opt Options) {
	tl := make(map[uint64]*TrackTimeline, len(r.Tracks))
	for i := range r.Tracks {
		tl[r.Tracks[i].Track] = &r.Tracks[i]
	}
	anchor := tl[r.Anchor]
	if anchor == nil || len(anchor.Samples) == 0 {
		r.Faults.Addf(media.LevelDegraded, media.CodeSyncAnchor, r.Anchor, 0,
			"anchor track %d has no samples", r.Anchor)
		return
	}
	var diffs []int64
	for i := range r.Tracks {
		t := &r.Tracks[i]
		if t.Track == r.Anchor || len(t.Samples) == 0 {
			continue
		}
		for _, s := range t.Samples {
			d := s.TimeNs - nearestSample(anchor.Samples, s.TimeNs)
			if d < 0 {
				d = -d
			}
			diffs = append(diffs, d)
		}
	}
	if len(diffs) == 0 {
		return
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i] < diffs[j] })
	r.AVSkewP95Ns = percentile(diffs, 95)
	r.AVSkewMaxNs = diffs[len(diffs)-1]
	var sum int64
	for _, d := range diffs {
		sum += d
	}
	r.AVSkewMeanNs = sum / int64(len(diffs))
	if r.AVSkewMaxNs > opt.MaxSkewNs {
		r.Faults.Addf(media.LevelDegraded, media.CodeSyncMismatch, 0, 0,
			"max cross-track skew %d ns exceeds %d ns", r.AVSkewMaxNs, opt.MaxSkewNs)
	}
}

// nearestSample returns the anchor timestamp closest to ts.
func nearestSample(samples []Sample, ts int64) int64 {
	if len(samples) == 0 {
		return ts
	}
	i := sort.Search(len(samples), func(i int) bool { return samples[i].TimeNs >= ts })
	if i == 0 {
		return samples[0].TimeNs
	}
	if i == len(samples) {
		return samples[len(samples)-1].TimeNs
	}
	if ts-samples[i-1].TimeNs <= samples[i].TimeNs-ts {
		return samples[i-1].TimeNs
	}
	return samples[i].TimeNs
}

// percentile returns the p-th percentile of sorted values (nearest rank).
func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	idx := (p*len(sorted) + 99) / 100
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}

// CueDrift returns the signed offset of each cue sample from its nearest
// video sample, i.e. the "action-hint misalignment" the scenario reports.
func (r *Result) CueDrift() []int64 {
	var video, cue []int64
	for i := range r.Tracks {
		switch r.Tracks[i].Kind {
		case media.KindVideo:
			for _, s := range r.Tracks[i].Samples {
				video = append(video, s.TimeNs)
			}
		case media.KindCue:
			for _, s := range r.Tracks[i].Samples {
				cue = append(cue, s.TimeNs)
			}
		}
	}
	if len(video) == 0 || len(cue) == 0 {
		return nil
	}
	sort.Slice(video, func(i, j int) bool { return video[i] < video[j] })
	out := make([]int64, 0, len(cue))
	for _, ts := range cue {
		out = append(out, ts-nearestTs(video, ts))
	}
	return out
}

func nearestTs(sorted []int64, ts int64) int64 {
	i := sort.Search(len(sorted), func(i int) bool { return sorted[i] >= ts })
	if i == 0 {
		return sorted[0]
	}
	if i == len(sorted) {
		return sorted[len(sorted)-1]
	}
	if ts-sorted[i-1] <= sorted[i]-ts {
		return sorted[i-1]
	}
	return sorted[i]
}

// Track returns the timeline for one track.
func (r *Result) Track(n uint64) (*TrackTimeline, bool) {
	for i := range r.Tracks {
		if r.Tracks[i].Track == n {
			return &r.Tracks[i], true
		}
	}
	return nil, false
}
