// Package track maps parsed elementary streams onto tracks, converts their
// timestamps onto a shared timeline, and explicitly handles timestamp
// wraparound, duplicate keyframes and non-monotonic indexes.
package track

import (
	"fmt"
	"strconv"
	"strings"

	"mediapack/internal/clock"
	"mediapack/internal/codec"
	"mediapack/internal/index"
)

// Kind is the track media type.
type Kind int

const (
	Video Kind = iota
	Audio
)

// Track is one mapped timeline.
type Track struct {
	ID       uint32
	Kind     Kind
	Codec    codec.Codec
	Timebase clock.Rational
	Units    []codec.AccessUnit
	// UnwrappedPTS holds 90kHz-domain PTS extended past 32-bit wraparound.
	UnwrappedPTS []int64
	KeyframeSeqs []int64
	Anomalies    []string
	// WrapCount is the number of 32-bit timestamp wrap events reconstructed.
	WrapCount int
}

// TimelineSummary is the deterministic digest of a mapping run.
type TimelineSummary struct {
	Tracks        int      `json:"tracks"`
	TotalAU       int      `json:"total_au"`
	Keyframes     int      `json:"keyframes"`
	FirstPTSMs    int64    `json:"first_pts_ms"`
	LastPTSMs     int64    `json:"last_pts_ms"`
	DurationMs    int64    `json:"duration_ms"`
	WrapCount     int      `json:"wrap_count"`
	DuplicateKeys int      `json:"duplicate_keyframes"`
	NonMonotonic  int      `json:"non_monotonic_records"`
	Anomalies     []string `json:"anomalies"`
}

// Map builds a track from parsed units, resolving RTP-style 32-bit timestamp
// wraparound and merging index anomaly reports into the track record.
func Map(id uint32, kind Kind, c codec.Codec, units []codec.AccessUnit,
	entries []index.Entry, reports []index.Report) *Track {

	t := &Track{
		ID:       id,
		Kind:     kind,
		Codec:    c,
		Timebase: clock.RTPClock(),
		Units:    units,
	}
	// Reconstruct the unwrapped axis from the low 32 bits, exactly as a
	// transport receiver seeing wrapping RTP timestamps would do. Each time
	// the unwrapped value crosses a full 2^32 boundary counts as one wrap.
	const mod32 = int64(1) << 32
	var prev int64
	for i, au := range units {
		wrapped := uint32(au.PTS)
		cur := clock.Unwrap32(prev, wrapped)
		if i > 0 && cur/mod32 > prev/mod32 {
			t.WrapCount++
		}
		t.UnwrappedPTS = append(t.UnwrappedPTS, cur)
		prev = cur
		if au.Key {
			t.KeyframeSeqs = append(t.KeyframeSeqs, au.Seq)
		}
	}
	// Reconcile the external keyframe index: duplicates are de-duplicated;
	// non-monotonic records are surfaced as recoverable anomalies.
	seen := map[int64]bool{}
	for _, e := range entries {
		if seen[e.FrameSeq] {
			t.Anomalies = append(t.Anomalies,
				fmt.Sprintf("duplicate keyframe index entry seq=%d off=%d dropped", e.FrameSeq, e.Offset))
			continue
		}
		seen[e.FrameSeq] = true
	}
	for _, r := range reports {
		t.Anomalies = append(t.Anomalies,
			fmt.Sprintf("index record %d: %s (%s)", r.Record, r.Anomaly, r.Detail))
	}
	t.Anomalies = append(t.Anomalies,
		fmt.Sprintf("wraparound_events=%d", t.WrapCount))
	return t
}

// Summarize renders the shared timeline across all mapped tracks.
func Summarize(tracks []*Track) TimelineSummary {
	s := TimelineSummary{Tracks: len(tracks)}
	var first, last int64 = 1<<62, -(1 << 62)
	for _, t := range tracks {
		s.TotalAU += len(t.Units)
		s.Keyframes += len(t.KeyframeSeqs)
		s.WrapCount += t.WrapCount
		for _, a := range t.Anomalies {
			s.Anomalies = append(s.Anomalies, a)
			switch {
			case strings.Contains(a, "duplicate keyframe"):
				s.DuplicateKeys++
			case strings.Contains(a, "non_monotonic"):
				s.NonMonotonic++
			}
		}
		if len(t.UnwrappedPTS) > 0 {
			if t.UnwrappedPTS[0] < first {
				first = t.UnwrappedPTS[0]
			}
			if t.UnwrappedPTS[len(t.UnwrappedPTS)-1] > last {
				last = t.UnwrappedPTS[len(t.UnwrappedPTS)-1]
			}
		}
	}
	if s.TotalAU > 0 {
		s.FirstPTSMs = clock.Rescale(first, clock.RTPClock(), clock.Millis())
		s.LastPTSMs = clock.Rescale(last, clock.RTPClock(), clock.Millis())
		s.DurationMs = s.LastPTSMs - s.FirstPTSMs
	}
	return s
}

// ParseWrapCount is exposed for tests that assert on serialized summaries.
func ParseWrapCount(s string) int {
	const key = "wraparound_events="
	i := strings.Index(s, key)
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(s[i:], key))
	return n
}
