// Package fanout fans one recovered shared timeline out into multiple bitrate
// representations and fans them back in as a single aligned manifest. Every
// representation is measured from the recovered sample bytes, so the reported
// bitrate reflects what survived recovery rather than a nominal target.
package fanout

import (
	"fmt"
	"sort"

	"mediacheck/internal/clock"
	"mediacheck/internal/fault"
	"mediacheck/internal/probe"
	"mediacheck/internal/repair"
)

// Config controls manifest generation.
type Config struct {
	// LadderBPS lists target bitrates in ascending order.
	LadderBPS []int64
	// SegmentMS is the shared segment duration.
	SegmentMS int64
	// ToleranceMS is the cross-layer boundary tolerance.
	ToleranceMS int64
	// MaxRepresentations caps the ladder length.
	MaxRepresentations int
	// MinSegmentBytes is the smallest payload a segment may claim before its
	// measurement is flagged as unstable.
	MinSegmentBytes int64
}

// DefaultConfig returns a 300/600 kbps ladder over 2 s segments.
func DefaultConfig() Config {
	return Config{
		LadderBPS:          []int64{300_000, 600_000},
		SegmentMS:          2000,
		ToleranceMS:        1,
		MaxRepresentations: 16,
		MinSegmentBytes:    1,
	}
}

// Normalize sorts and deduplicates the ladder, drops non-positive entries and
// rejects a ladder longer than the cap.
func (c Config) Normalize() (Config, error) {
	d := DefaultConfig()
	if c.ToleranceMS <= 0 {
		c.ToleranceMS = d.ToleranceMS
	}
	if c.MaxRepresentations <= 0 {
		c.MaxRepresentations = d.MaxRepresentations
	}
	if c.MinSegmentBytes <= 0 {
		c.MinSegmentBytes = d.MinSegmentBytes
	}
	if c.SegmentMS <= 0 {
		c.SegmentMS = d.SegmentMS
	}
	if len(c.LadderBPS) == 0 {
		c.LadderBPS = d.LadderBPS
	}
	seen := make(map[int64]bool, len(c.LadderBPS))
	out := make([]int64, 0, len(c.LadderBPS))
	for _, v := range c.LadderBPS {
		if v <= 0 || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return Config{}, fmt.Errorf("ladder must contain a positive bitrate")
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) > c.MaxRepresentations {
		out = out[:c.MaxRepresentations]
	}
	c.LadderBPS = out
	return c, nil
}

// Representation is one ladder rung over the shared timeline.
type Representation struct {
	Name           string       `json:"name"`
	TargetBPS      int64        `json:"target_bps"`
	TrackID        uint32       `json:"track_id"`
	Kind           probe.Kind   `json:"kind"`
	Segments       []RepSegment `json:"segments"`
	TargetBytes    float64      `json:"target_bytes"`
	ActualBytes    int64        `json:"actual_bytes"`
	ActualBPS      float64      `json:"actual_bps"`
	DeltaPct       float64      `json:"bitrate_delta_pct"`
	DurationMS     int64        `json:"duration_ms"`
	QualitySamples int          `json:"quality_valid_samples"`
	Keyframes      int          `json:"keyframes"`
}

// RepSegment is one representation segment.
type RepSegment struct {
	Index         int     `json:"index"`
	StartMS       int64   `json:"start_ms"`
	EndMS         int64   `json:"end_ms"`
	DurationMS    int64   `json:"duration_ms"`
	Bytes         int64   `json:"bytes"`
	Samples       int     `json:"samples"`
	Keyframes     int     `json:"keyframes"`
	TargetBPS     int64   `json:"target_bps"`
	ActualBPS     float64 `json:"actual_bps"`
	DeltaPct      float64 `json:"bitrate_delta_pct"`
	Aligned       bool    `json:"aligned"`
	Unrecoverable bool    `json:"unrecoverable,omitempty"`
}

// Manifest is the aligned, machine-readable summary.
type Manifest struct {
	SegmentMS           int64             `json:"segment_ms"`
	ToleranceMS         int64             `json:"tolerance_ms"`
	Renditions          []*Representation `json:"renditions"`
	AlignmentMS         int64             `json:"alignment_ms"`
	Switchable          bool              `json:"switchable"`
	TargetBPS           int64             `json:"target_bps"`
	ActualBPS           float64           `json:"actual_bps"`
	DeltaPct            float64           `json:"bitrate_delta_pct"`
	TotalBytes          int64             `json:"total_bytes"`
	DurationMS          int64             `json:"duration_ms"`
	QualityValidSamples int               `json:"quality_valid_samples"`
	Keyframes           int               `json:"keyframes"`
	Reasons             []string          `json:"reasons,omitempty"`
	Audio               *AudioSummary     `json:"audio,omitempty"`
	RTP                 *RTPSummary       `json:"rtp,omitempty"`
	Issues              []*fault.Fault    `json:"-"`
}

// Build derives the ladder for a single video representation of a recovered
// track. The caller picks which track carries the ladder (normally video).
func Build(track *repair.TrackResult, cfg Config) (*Manifest, error) {
	norm, err := cfg.Normalize()
	if err != nil {
		return nil, err
	}
	m := &Manifest{
		SegmentMS:   norm.SegmentMS,
		ToleranceMS: norm.ToleranceMS,
	}
	if track == nil || track.Recovered.Samples == 0 {
		return nil, fmt.Errorf("no recoverable video samples for the ladder")
	}
	for _, bps := range norm.LadderBPS {
		rep := buildRep(track, bps, norm)
		m.Renditions = append(m.Renditions, rep)
		m.TotalBytes += rep.ActualBytes
		m.QualityValidSamples += rep.QualitySamples
		if rep.Keyframes > m.Keyframes {
			m.Keyframes = rep.Keyframes
		}
		if rep.DurationMS > m.DurationMS {
			m.DurationMS = rep.DurationMS
		}
	}
	m.ActualBPS = bitsPerSecond(m.TotalBytes, m.DurationMS)
	m.DeltaPct = deltaPct(m.ActualBPS, float64(m.TargetBPS))
	m.TargetBPS = norm.LadderBPS[len(norm.LadderBPS)-1]
	m.AlignmentMS = segmentAlignment(m.Renditions)
	m.Switchable = m.AlignmentMS <= norm.ToleranceMS
	if !m.Switchable {
		if m.AlignmentMS > norm.ToleranceMS {
			m.Reasons = append(m.Reasons, fmt.Sprintf(
				"segment boundaries disagree by %d ms (> tolerance %d ms)", m.AlignmentMS, norm.ToleranceMS))
		}
		if !track.Recovered.Complete {
			m.Reasons = append(m.Reasons, "recovery skipped damaged samples; the ladder is not uniformly switchable")
		}
	}
	return m, nil
}

// buildRep applies the target bitrate to each recovered bucket. Bytes are the
// measured sample bytes for that bucket; target bytes are the nominal budget.
func buildRep(track *repair.TrackResult, bps int64, cfg Config) *Representation {
	rep := &Representation{
		Name:       fmt.Sprintf("%dk", bps/1000),
		TargetBPS:  bps,
		TrackID:    track.TrackID,
		Kind:       track.Kind,
		DurationMS: track.Recovered.EndMS - track.Recovered.StartMS,
		Keyframes:  track.Recovered.Keyframes,
	}
	if rep.DurationMS <= 0 {
		rep.DurationMS = track.DurationMS
	}
	for _, seg := range track.Segments {
		if seg.Samples == 0 {
			continue
		}
		dur := seg.DurationMS
		if dur <= 0 {
			dur = cfg.SegmentMS
		}
		actual := bitsPerSecond(seg.Bytes, dur)
		target := float64(bps) * float64(dur) / 1000.0 / 8.0
		rs := RepSegment{
			Index:      seg.Index,
			StartMS:    seg.StartMS,
			EndMS:      seg.StartMS + dur,
			DurationMS: dur,
			Bytes:      seg.Bytes,
			Samples:    seg.Samples,
			Keyframes:  seg.Keyframes,
			TargetBPS:  bps,
			ActualBPS:  actual,
			DeltaPct:   deltaPct(actual, float64(bps)),
			Aligned:    true,
		}
		if seg.Bytes < cfg.MinSegmentBytes {
			rs.Unrecoverable = true
		}
		rep.Segments = append(rep.Segments, rs)
		rep.ActualBytes += seg.Bytes
		rep.TargetBytes += target
		rep.QualitySamples += seg.Samples
	}
	rep.ActualBPS = bitsPerSecond(rep.ActualBytes, rep.DurationMS)
	rep.DeltaPct = deltaPct(rep.ActualBPS, float64(bps))
	return rep
}

// segmentAlignment measures the largest start-time disagreement at a shared
// segment index across renditions.
func segmentAlignment(reps []*Representation) int64 {
	if len(reps) < 2 {
		return 0
	}
	// Every rendition shares one track, so identical recovered durations map
	// to identical boundaries. Disagreement here means the fan-out changed
	// the timeline, which is a bug worth surfacing rather than hiding.
	// Index each rendition's segments by bucket.
	type key struct{ idx int }
	byIdx := map[key][]int64{}
	for _, r := range reps {
		for _, s := range r.Segments {
			byIdx[key{s.Index}] = append(byIdx[key{s.Index}], s.StartMS)
		}
	}
	var max int64
	for _, starts := range byIdx {
		if len(starts) < 2 {
			continue
		}
		mn, mx := starts[0], starts[0]
		for _, v := range starts[1:] {
			if v < mn {
				mn = v
			}
			if v > mx {
				mx = v
			}
		}
		if d := clock.AbsDiff(mx, mn); d > max {
			max = d
		}
	}
	return max
}

// bitsPerSecond converts a byte count over a millisecond span.
func bitsPerSecond(bytes, durationMS int64) float64 {
	if durationMS <= 0 {
		return 0
	}
	return float64(bytes) * 8 * 1000 / float64(durationMS)
}

// deltaPct returns the signed deviation from a target in percent. A zero
// target yields 0 rather than an infinity so the summary stays finite.
func deltaPct(actual, target float64) float64 {
	if target == 0 {
		return 0
	}
	return (actual - target) / target * 100
}
