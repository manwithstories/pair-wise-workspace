package syncer

import (
	"sort"

	"pairflow/internal/model"
)

// FramePoint is one mapped frame on the shared nanosecond timeline.
type FramePoint struct {
	TrackID    string
	Sequence   int64
	PTSNS      int64
	DurationNS int64
}

// AudioGrid is a PCM sample clock on the same timeline.
type AudioGrid struct {
	SampleRateHz int
	FirstPTSNS   int64
}

// SamplePTS returns the ideal time of sample index n.
func (a AudioGrid) SamplePTS(n int64) int64 {
	if a.SampleRateHz <= 0 {
		return a.FirstPTSNS
	}
	// Integer rounding avoids floating-point ambiguity in output hashes.
	q := n * int64(model.TimebaseHz)
	den := int64(a.SampleRateHz)
	if q >= 0 {
		return a.FirstPTSNS + (q+den/2)/den
	}
	return a.FirstPTSNS - ((-q + den/2) / den)
}

// NearestSampleDistanceNS returns the distance to the closest PCM sample.
func (a AudioGrid) NearestSampleDistanceNS(pts int64) int64 {
	if a.SampleRateHz <= 0 {
		return 0
	}
	delta := pts - a.FirstPTSNS
	n := delta * int64(a.SampleRateHz) / int64(model.TimebaseHz)
	best := int64(-1)
	for _, cand := range []int64{n - 1, n, n + 1} {
		t := a.SamplePTS(cand)
		d := pts - t
		if d < 0 {
			d = -d
		}
		if best < 0 || d < best {
			best = d
		}
	}
	return best
}

// Gap is a synchronized missing interval propagated from transport.
type Gap struct {
	TrackID   string
	Reason    string
	Sequences []uint16
	Interval  model.Interval
}

// Report contains alignment quality and propagated gaps.
type Report struct {
	AlignedFrames int              `json:"aligned_frames"`
	TrackCount    int              `json:"track_count"`
	MaxSkewNS     int64            `json:"max_skew_ns"`
	MaxSkewUS     int64            `json:"max_skew_us"`
	MaxAudioNS    int64            `json:"max_audio_alignment_ns"`
	Gaps          []Gap            `json:"gaps"`
	GapRanges     []model.Interval `json:"gap_ranges"`
}

// Align maps variable-rate video points and audio to one timeline. Points are
// grouped by source sequence; no nominal frame rate is invented.
func Align(points []FramePoint, audio AudioGrid, gaps []Gap) *Report {
	rep := &Report{}
	bySeq := map[int64][]FramePoint{}
	tracks := map[string]struct{}{}
	var maxSkew, maxAudio int64

	for _, p := range points {
		bySeq[p.Sequence] = append(bySeq[p.Sequence], p)
		tracks[p.TrackID] = struct{}{}
		if d := audio.NearestSampleDistanceNS(p.PTSNS); d > maxAudio {
			maxAudio = d
		}
	}
	rep.TrackCount = len(tracks)

	seqs := make([]int64, 0, len(bySeq))
	for seq := range bySeq {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })

	for _, seq := range seqs {
		group := bySeq[seq]
		if len(group) == 0 {
			continue
		}
		minPTS, maxPTS := group[0].PTSNS, group[0].PTSNS
		for _, p := range group[1:] {
			if p.PTSNS < minPTS {
				minPTS = p.PTSNS
			}
			if p.PTSNS > maxPTS {
				maxPTS = p.PTSNS
			}
		}
		skew := maxPTS - minPTS
		if skew < 0 {
			skew = -skew
		}
		if skew > maxSkew {
			maxSkew = skew
		}
		rep.AlignedFrames += len(group)
	}

	rep.MaxSkewNS = maxSkew
	// Ceiling conversion ensures a reported sub-microsecond residual is 1us
	// rather than being hidden as zero when it is nonzero.
	rep.MaxSkewUS = (maxSkew + 999) / 1000
	if maxAudio > maxSkew {
		rep.MaxSkewNS = maxAudio
		rep.MaxSkewUS = (maxAudio + 999) / 1000
	}
	rep.MaxAudioNS = maxAudio
	rep.Gaps = append([]Gap(nil), gaps...)
	for _, g := range gaps {
		rep.GapRanges = append(rep.GapRanges, g.Interval)
	}
	return rep
}
