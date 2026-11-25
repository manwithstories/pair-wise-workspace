// Package report turns a timeline result into a deterministic manifest with
// bitrate, quality and alignment statistics, and commits it atomically.
package report

import (
	"fmt"
	"sort"

	"avcheck/internal/media"
	"avcheck/internal/timeline"
)

// ManifestVersion is bumped when the JSON shape changes.
const ManifestVersion = 1

// Options carry the run parameters that affect reported numbers.
type Options struct {
	TargetBps  int64
	DurationNs int64
	Seed       int64
	MemoryCap  int64
}

// Quality groups the acceptance metrics.
type Quality struct {
	VideoGapPct         float64 `json:"video_gap_pct"`
	AudioGapMs          float64 `json:"audio_gap_ms"`
	RecoveredPct        float64 `json:"recovered_payload_pct"`
	RecoveredPayloadPct float64 `json:"-"`
	RecoveredPackets    int     `json:"recovered_packets"`
	Unrecovered         int     `json:"unrecovered_packets"`
	JitterGaps          int     `json:"jitter_gaps"`
	Duplicates          int     `json:"duplicates"`
}

// TrackReport is the per-track summary.
type TrackReport struct {
	Track     uint64 `json:"track"`
	Kind      string `json:"kind"`
	Samples   int    `json:"samples"`
	FirstNs   int64  `json:"first_ns"`
	LastNs    int64  `json:"last_ns"`
	Gaps      int    `json:"gaps"`
	Recovered int    `json:"recovered"`
	Monotonic bool   `json:"monotonic"`
}

// Manifest is the committed JSON document. Field order is fixed so equal
// seeds produce byte-identical output.
type Manifest struct {
	Version          int           `json:"version"`
	Seed             int64         `json:"seed"`
	TargetBps        int64         `json:"target_bps"`
	ActualBps        int64         `json:"actual_bps"`
	DeltaPct         float64       `json:"delta_pct"`
	PayloadBytes     int64         `json:"payload_bytes"`
	DurationNs       int64         `json:"duration_ns"`
	RecoveredPackets int           `json:"recovered_packets"`
	Quality          Quality       `json:"quality"`
	AVSkewP95Ms      float64       `json:"av_skew_p95_ms"`
	AVSkewMaxMs      float64       `json:"av_skew_max_ms"`
	ManifestDeltaMs  float64       `json:"manifest_delta_ms"`
	PeakBufferBytes  int64         `json:"peak_buffer_bytes"`
	MemoryCap        int64         `json:"memory_cap_bytes"`
	Anchor           uint64        `json:"anchor_track"`
	FlushOrder       []uint64      `json:"flush_order"`
	Tracks           []TrackReport `json:"tracks"`
	SHA256           string        `json:"sha256,omitempty"`
	Faults           []FaultReport `json:"faults"`
}

// FaultReport is the stable, printable view of one fault.
type FaultReport struct {
	Level  string `json:"level"`
	Code   string `json:"code"`
	Track  uint64 `json:"track,omitempty"`
	Offset int64  `json:"offset,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Build computes the manifest from an aligned result. The payload byte count
// and peak buffer figure come from the timeline/transport counters carried on
// the result; missing counters leave the metric at zero rather than guessing.
func Build(res *timeline.Result, opt Options) (*Manifest, error) {
	if res == nil {
		return nil, fmt.Errorf("report: nil timeline result")
	}
	if res.Faults == nil {
		return nil, fmt.Errorf("report: timeline result has no fault model")
	}
	m := &Manifest{
		Version:     ManifestVersion,
		Seed:        opt.Seed,
		TargetBps:   opt.TargetBps,
		DurationNs:  opt.DurationNs,
		MemoryCap:   opt.MemoryCap,
		Anchor:      res.Anchor,
		FlushOrder:  append([]uint64(nil), res.CueOrder...),
		AVSkewP95Ms: nsToMs(res.AVSkewP95Ns),
		AVSkewMaxMs: nsToMs(res.AVSkewMaxNs),
	}
	var peak int64
	var totalBytes int64
	for i := range res.Tracks {
		t := &res.Tracks[i]
		tr := TrackReport{
			Track: t.Track, Kind: t.Kind.String(), Samples: len(t.Samples),
			FirstNs: t.FirstNs, LastNs: t.LastNs, Gaps: t.Gaps,
			Recovered: t.Recovered, Monotonic: t.Monotonic,
		}
		m.Tracks = append(m.Tracks, tr)
		m.Quality.JitterGaps += t.Gaps
		switch t.Kind {
		case media.KindVideo:
			m.Quality.VideoGapPct += gapPct(t.Gaps, len(t.Samples))
		case media.KindAudio:
			m.Quality.AudioGapMs += float64(t.Gaps) * avgStepMs(t)
		}
	}
	if res.PeakBufferBytes > peak {
		peak = res.PeakBufferBytes
	}
	m.PeakBufferBytes = peak
	totalBytes = res.PayloadBytes
	m.PayloadBytes = totalBytes
	m.RecoveredPackets = res.RecoveredPackets
	m.Quality.RecoveredPackets = res.RecoveredPackets
	m.Quality.Unrecovered = res.UnrecoveredPackets
	m.Quality.Duplicates = res.Duplicates
	m.Quality.RecoveredPct = res.RecoveredPct
	m.Quality.RecoveredPayloadPct = res.RecoveredPct
	m.ManifestDeltaMs = nsToMs(res.ManifestDeltaNs)
	if opt.DurationNs > 0 {
		m.ActualBps = totalBytes * 8 * int64(1e9) / opt.DurationNs
	}
	if opt.TargetBps > 0 {
		m.DeltaPct = float64(m.ActualBps-opt.TargetBps) * 100 / float64(opt.TargetBps)
	}
	for _, f := range res.Faults.List {
		m.Faults = append(m.Faults, FaultReport{
			Level: f.Level.String(), Code: string(f.Code), Track: f.Track,
			Offset: f.Offset, Detail: f.Detail,
		})
	}
	// Fault order must be fully deterministic: equal (code, offset) pairs are
	// broken by every remaining field and exact duplicates are collapsed.
	sort.SliceStable(m.Faults, func(i, j int) bool {
		a, b := m.Faults[i], m.Faults[j]
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if a.Offset != b.Offset {
			return a.Offset < b.Offset
		}
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		if a.Level != b.Level {
			return a.Level < b.Level
		}
		return a.Detail < b.Detail
	})
	m.Faults = dedupeFaults(m.Faults)
	if m.Faults == nil {
		m.Faults = []FaultReport{}
	}
	return m, nil
}

func nsToMs(ns int64) float64 { return float64(ns) / 1e6 }

// dedupeFaults drops adjacent exact duplicates produced when several stages
// observe the same fault.
func dedupeFaults(in []FaultReport) []FaultReport {
	if len(in) == 0 {
		return in
	}
	out := in[:1]
	for _, f := range in[1:] {
		if f == out[len(out)-1] {
			continue
		}
		out = append(out, f)
	}
	return out
}

// gapPct is gaps/samples as a percentage (0 when there are no samples).
func gapPct(gaps, samples int) float64 {
	if samples == 0 {
		return 0
	}
	return float64(gaps) * 100 / float64(samples)
}

// avgStepMs estimates the nominal inter-sample interval from the timeline.
func avgStepMs(t *timeline.TrackTimeline) float64 {
	if len(t.Samples) < 2 {
		return 0
	}
	span := t.LastNs - t.FirstNs
	if span <= 0 {
		return 0
	}
	return float64(span) / float64(len(t.Samples)-1) / 1e6
}
