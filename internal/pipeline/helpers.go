package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"avrepair/internal/media"
)

func appendUniq(list []string, v string) []string {
	if contains(list, v) {
		return list
	}
	return append(list, v)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	idx := int(p*float64(len(sorted)-1) + 0.5)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func findSegmentBySeq(t *media.Track, seq int) *media.Segment {
	for _, s := range t.Segments {
		if s.Seq == seq {
			return s
		}
	}
	return nil
}

func segStep(seg *media.Segment) media.Rational {
	if len(seg.Samples) >= 2 {
		if d := seg.Samples[1].PTS.Sub(seg.Samples[0].PTS); d.Cmp(media.Zero) > 0 {
			return d
		}
	}
	if len(seg.Samples) > 0 && seg.Samples[0].Duration.Cmp(media.Zero) > 0 {
		return seg.Samples[0].Duration
	}
	return media.NewRational(33, 1000)
}

func durationOf(seg *media.Segment) media.Rational {
	step := segStep(seg)
	last := seg.Samples[len(seg.Samples)-1].PTS
	first := seg.Samples[0].PTS
	d := last.Sub(first).Add(step)
	if seg.Duration.Cmp(d) > 0 && seg.Duration.Cmp(media.Zero) > 0 {
		return seg.Duration
	}
	return d
}

func strictlyIncreasing(samples []media.Sample) bool {
	for i := 1; i < len(samples); i++ {
		if samples[i].PTS.Cmp(samples[i-1].PTS) <= 0 {
			return false
		}
	}
	return true
}

func trackIncreasing(track *media.Track) bool {
	var last *media.Rational
	for _, seg := range track.Segments {
		if !seg.Valid() {
			continue
		}
		if last != nil && seg.Samples[0].PTS.Cmp(*last) <= 0 {
			return false
		}
		p := seg.Samples[len(seg.Samples)-1].PTS
		last = &p
	}
	return true
}
