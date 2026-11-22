// Package verify computes archive quality and bitrate metrics.  Quality is
// measured as PSNR between the recovered payload and the source payload the
// fixture generated, sampled on visible frames only.  Metrics are pure
// functions of the inputs so a spilled run and an in-memory run agree.
package verify

import (
	"fmt"
	"math"
	"sort"

	"archivecheck/internal/timeline"
)

// Sample is one quality observation: a reconstructed frame against the
// reference it came from.
type Sample struct {
	Track   uint64
	Offset  int64
	Ref     int16
	Got     int16
	Weight  int
	Missing bool
}

// QualityStats is the PSNR summary.
type QualityStats struct {
	PSNRdB  float64 `json:"quality_psnr_db"`
	Samples int     `json:"quality_samples"`
	MSE     float64 `json:"quality_mse"`
	Missing int     `json:"quality_missing_frames"`
}

// PSNR computes peak signal-to-noise ratio over 16-bit samples.  An exact
// match yields 99 dB, a finite sentinel that serializes cleanly.
func PSNR(samples []Sample) QualityStats {
	var st QualityStats
	var sse float64
	var n int
	for _, s := range samples {
		if s.Missing {
			st.Missing++
			continue
		}
		w := s.Weight
		if w <= 0 {
			w = 1
		}
		d := float64(s.Got) - float64(s.Ref)
		sse += float64(w) * d * d
		n += w
	}
	st.Samples = n
	if n == 0 {
		return st
	}
	st.MSE = sse / float64(n)
	if st.MSE == 0 {
		st.PSNRdB = 99
		return st
	}
	peak := 32767.0
	st.PSNRdB = 10 * math.Log10(peak*peak/st.MSE)
	return st
}

// Bitrate compares a target with the bytes actually produced over duration.
type Bitrate struct {
	TargetBps      int64   `json:"target_bps"`
	ActualBps      int64   `json:"actual_bps"`
	DeltaPct       float64 `json:"bitrate_delta_pct"`
	DurationMillis float64 `json:"duration_ms"`
	ActualBytes    int64   `json:"actual_bytes"`
	WithinTarget   bool    `json:"within_target"`
}

// TargetTolerance is the allowed absolute bitrate deviation (5%).
const TargetTolerance = 0.05

// ComputeBitrate derives actual bitrate from bytes and exact duration and
// compares it with the target.
func ComputeBitrate(targetBps int64, bytes int64, durationSeconds float64) Bitrate {
	b := Bitrate{TargetBps: targetBps, ActualBytes: bytes, DurationMillis: durationSeconds * 1000}
	if durationSeconds > 0 {
		b.ActualBps = int64(math.Round(float64(bytes) * 8 / durationSeconds))
	}
	if targetBps > 0 {
		b.DeltaPct = 100 * (float64(b.ActualBps) - float64(targetBps)) / float64(targetBps)
	}
	b.WithinTarget = math.Abs(b.DeltaPct) <= TargetTolerance*100
	return b
}

// TrackBitrate summarises one track's visible payload rate.
type TrackBitrate struct {
	Track      uint64  `json:"track"`
	Bytes      int64   `json:"bytes"`
	Bps        int64   `json:"bps"`
	DurationMs float64 `json:"duration_ms"`
	Frames     int     `json:"frames"`
}

// Tracks computes per-track bitrates over each track's own extent.
func Tracks(tracks map[uint64]*timeline.TrackInfo, order []uint64) []TrackBitrate {
	out := make([]TrackBitrate, 0, len(order))
	for _, n := range order {
		info := tracks[n]
		if info == nil {
			continue
		}
		start, end := info.StartEnd()
		dur := end.Sub(start).Float64()
		tb := TrackBitrate{Track: n, Bytes: info.FrameBytes(), Frames: info.Visible(), DurationMs: dur * 1000}
		if dur > 0 {
			tb.Bps = int64(math.Round(float64(tb.Bytes) * 8 / dur))
		}
		out = append(out, tb)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Track < out[j].Track })
	return out
}

// Stats bundles every reported metric into a single comparable struct.
type Stats struct {
	Quality          QualityStats   `json:"quality"`
	Bitrate          Bitrate        `json:"bitrate"`
	Tracks           []TrackBitrate `json:"tracks"`
	LostPackets      int            `json:"lost_packets"`
	DuplicatePackets int            `json:"duplicate_packets"`
	ReorderedPackets int            `json:"reordered_packets"`
	TimestampRepairs int            `json:"timestamp_repairs"`
	IndexEntries     int            `json:"index_entries"`
	IndexSHA256      string         `json:"index_sha256"`
	ManifestAlignMS  float64        `json:"manifest_alignment_ms"`
	RollbackCount    int            `json:"rollback_count"`
	PacketLossRate   float64        `json:"packet_loss_rate"`
}

// Signature returns the fields that must match between a spilled run and an
// in-memory run on the same input.  It deliberately excludes timing and
// memory figures.
func (s Stats) Signature() string {
	return fmt.Sprintf("psnr=%.6f samples=%d mse=%.6f lost=%d dup=%d reorder=%d repairs=%d idx=%d sha=%s align=%.6f",
		s.Quality.PSNRdB, s.Quality.Samples, s.Quality.MSE,
		s.LostPackets, s.DuplicatePackets, s.ReorderedPackets,
		s.TimestampRepairs, s.IndexEntries, s.IndexSHA256, s.ManifestAlignMS)
}

// Deterministic reports whether two stats structs agree on the fields that
// must be invariant under spill/no-spill and rerun.
func Deterministic(a, b Stats) bool {
	return a.Signature() == b.Signature()
}
