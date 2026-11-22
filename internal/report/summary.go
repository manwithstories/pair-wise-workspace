// Package report prints the quantified run summary as key=value lines.
package report

import (
	"fmt"
	"io"
	"math"

	"mediaprobe/internal/fingerprint"
)

// SegmentEntry is one cut segment's report row.
type SegmentEntry struct {
	ID          int
	Start, End  int64 // media clock
	Hash        [32]byte
	DuplicateOf int // -1 when unique
	Uncertain   bool
}

// Summary aggregates all run metrics.
type Summary struct {
	RoundtripPct       float64
	KeyframeHitPct     float64
	Segments           []SegmentEntry
	QueuePeak          int
	BackpressureEvents int
	DriftPPM           float64
	SeqGaps            int
	SeqDuplicates      int
	SeqOutOfOrder      int
	TFDTRegressions    int
	ForwardGaps        int
	UncertainIntervals int
	SilenceIntervals   int
	UnknownIntervals   int
	DuplicateSegments  int
	CorruptFragments   int
	TooLargeFragments  int
}

// UniqueHashes counts distinct segment hashes.
func (s *Summary) UniqueHashes() int {
	seen := map[[32]byte]bool{}
	for _, e := range s.Segments {
		seen[e.Hash] = true
	}
	return len(seen)
}

// Print writes the summary in key=value form.
func (s *Summary) Print(w io.Writer) {
	p := func(format string, args ...interface{}) { fmt.Fprintf(w, format+"\n", args...) }
	p("roundtrip_consistency_pct=%.1f", s.RoundtripPct)
	p("keyframe_index_hit_pct=%.1f", s.KeyframeHitPct)
	p("segments=%d", len(s.Segments))
	p("segment_sha256_count=%d", len(s.Segments))
	p("unique_sha256_count=%d", s.UniqueHashes())
	p("queue_peak=%d", s.QueuePeak)
	p("backpressure_events=%d", s.BackpressureEvents)
	p("drift_ppm=%d", int64(math.Round(s.DriftPPM)))
	p("seq_gaps=%d", s.SeqGaps)
	p("seq_duplicates=%d", s.SeqDuplicates)
	p("seq_out_of_order=%d", s.SeqOutOfOrder)
	p("tfdt_regressions=%d", s.TFDTRegressions)
	p("forward_gaps=%d", s.ForwardGaps)
	p("uncertain_intervals=%d", s.UncertainIntervals)
	p("silence_intervals=%d", s.SilenceIntervals)
	p("unknown_intervals=%d", s.UnknownIntervals)
	p("duplicate_segments=%d", s.DuplicateSegments)
	p("corrupt_fragments=%d", s.CorruptFragments)
	p("too_large_fragments=%d", s.TooLargeFragments)
	for _, e := range s.Segments {
		line := fmt.Sprintf("segment[%d].sha256=%s", e.ID, fingerprint.Hex(e.Hash))
		if e.DuplicateOf >= 0 {
			line += fmt.Sprintf(" dup_of=%d", e.DuplicateOf)
		}
		if e.Uncertain {
			line += " uncertain=1"
		}
		fmt.Fprintln(w, line)
	}
}
