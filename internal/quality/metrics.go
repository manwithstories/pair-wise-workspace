// Package quality turns inspection and recovery results into delivery
// metrics: bitrate conformance, valid-sample ratio, damaged ranges, GOP
// quantiles and cross-representation alignment error.
package quality

import (
	"math"
	"sort"

	"mediac/internal/core"
	"mediac/internal/inspect"
	"mediac/internal/manifest"
	"mediac/internal/transport"
)

// Metrics is the per-representation quality verdict projected onto the
// unified millisecond clock.
type Metrics struct {
	RepID           string          `json:"rep_id"`
	TargetBPS       int64           `json:"target_bps"`
	ActualBPS       int64           `json:"actual_bps"`
	BitrateDeltaPct float64         `json:"bitrate_delta_pct"`
	ValidSamplePct  float64         `json:"valid_sample_pct"`
	DamagedRanges   []Range         `json:"damaged_ranges"`
	GOPQuantilesMs  map[int]float64 `json:"gop_quantiles_ms"`
	AlignmentMaxMs  int64           `json:"alignment_max_ms"`
	TotalSamples    int             `json:"total_samples"`
	ValidSamples    int             `json:"valid_samples"`
	DamagedSegments int             `json:"damaged_segments"`
}

// Range is a damaged span on the unified millisecond timeline plus its origin.
type Range struct {
	StartMs int64          `json:"start_ms"`
	EndMs   int64          `json:"end_ms"`
	Source  core.Source    `json:"source"`
	Kind    core.FaultKind `json:"kind"`
}

// Input bundles every upstream artifact needed for one representation.
type Input struct {
	Manifest  *manifest.Manifest
	RepIndex  int
	Reports   []*inspect.FragmentReport // in fragment order
	Recovered []transport.FragmentResult
	Faults    []core.Fault
}

// Compute derives all metrics for one representation. Manifest segment timing
// supplies the millisecond window; fragment indexes map onto segment indexes.
func Compute(in Input) Metrics {
	rep := in.Manifest.Representations[in.RepIndex]
	m := Metrics{
		RepID: rep.ID, TargetBPS: rep.TargetBPS,
		GOPQuantilesMs: map[int]float64{},
	}

	totalBytes, totalDurationMs, validSamples, totalSamples :=
		aggregateSamples(in.Reports, completenessSet(in.Recovered))
	m.TotalSamples, m.ValidSamples = totalSamples, validSamples
	if totalSamples > 0 {
		m.ValidSamplePct = round2(float64(validSamples) * 100 / float64(totalSamples))
	}

	// Actual bitrate from recovered/parsed payload over the manifest window.
	windowMs := manifestWindowMs(rep)
	if windowMs > 0 {
		m.ActualBPS = int64(math.Round(float64(totalBytes*8) * 1000 / float64(windowMs)))
	}
	if rep.TargetBPS > 0 {
		m.BitrateDeltaPct = round2(float64(m.ActualBPS-rep.TargetBPS) * 100 / float64(rep.TargetBPS))
	}
	_ = totalDurationMs

	m.DamagedRanges = collectDamagedRanges(rep, in)
	m.DamagedSegments = countDamagedSegments(m.DamagedRanges)
	m.GOPQuantilesMs = gopQuantiles(in.Reports)
	m.AlignmentMaxMs = alignmentError(in.Manifest, in.RepIndex)
	return m
}

// completenessSet indexes the fragments whose RTP recovery finished without
// an unrecovered gap.
func completenessSet(recovered []transport.FragmentResult) map[int]bool {
	out := map[int]bool{}
	for _, fr := range recovered {
		out[fr.Fragment] = fr.Complete
	}
	return out
}

// aggregateSamples sums byte counts and samples, treating samples in
// structurally sound, fully recovered, non-damaged fragments as valid.
func aggregateSamples(reports []*inspect.FragmentReport,
	complete map[int]bool) (totalBytes uint64, windowMs uint64, valid, total int) {
	for _, fr := range reports {
		structuralDamage := fr.Truncated || fr.HasInvalidNAL
		transportWhole := complete[fr.Fragment]
		// A truncated fragment may analyze fewer (or zero) samples than its trun
		// declares; count the declared expectation in the denominator so the
		// damage visibly lowers the valid-sample ratio.
		declared := fr.ExpectedSamples
		if declared < len(fr.Samples) {
			declared = len(fr.Samples)
		}
		if declared == 0 {
			declared = len(fr.Samples)
		}
		total += declared
		for _, s := range fr.Samples {
			totalBytes += uint64(s.Size)
			windowMs += uint64(core.Scale(int64(s.Duration), int64(fr.Timescale), 1000))
			if !structuralDamage && transportWhole && s.Duration > 0 {
				valid++
			}
		}
	}
	return totalBytes, windowMs, valid, total
}

func manifestWindowMs(rep manifest.Representation) int64 {
	if len(rep.Segments) == 0 {
		return 0
	}
	last := rep.Segments[len(rep.Segments)-1]
	return last.StartMs + last.DurationMs - rep.Segments[0].StartMs
}

// collectDamagedRanges merges structural faults and transport gaps onto the
// manifest's millisecond segment windows.
func collectDamagedRanges(rep manifest.Representation, in Input) []Range {
	seen := map[string]bool{}
	var out []Range
	add := func(segIdx int, kind core.FaultKind, src core.Source) {
		seg := findSegment(rep, segIdx)
		if seg == nil {
			return
		}
		key := rangeKey(seg.Index, kind)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Range{
			StartMs: seg.StartMs, EndMs: seg.EndMs(),
			Source: src, Kind: kind,
		})
	}
	// Transport-level burst gaps and late packets.
	for _, fr := range in.Recovered {
		if !fr.Complete {
			add(fr.Fragment, core.FaultBurstLoss,
				core.Source{RepID: rep.ID, SegmentIndex: fr.Fragment})
		}
	}
	// Structural bitstream faults.
	for _, rpt := range in.Reports {
		if rpt.Truncated {
			add(rpt.Fragment, core.FaultTruncatedBox,
				core.Source{RepID: rep.ID, SegmentIndex: rpt.Fragment})
		}
		if rpt.HasInvalidNAL {
			add(rpt.Fragment, core.FaultInvalidNAL,
				core.Source{RepID: rep.ID, SegmentIndex: rpt.Fragment})
		}
		if rpt.HasZeroDur {
			add(rpt.Fragment, core.FaultZeroDuration,
				core.Source{RepID: rep.ID, SegmentIndex: rpt.Fragment})
		}
	}
	// Explicit faults from the shared recorder (duplicates are warnings and do
	// not mark a range; damaged/blocking severities do).
	for _, f := range in.Faults {
		if f.Severity == core.SevDamaged || f.Severity == core.SevBlocking {
			add(f.Source.SegmentIndex, f.Kind, f.Source)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartMs != out[j].StartMs {
			return out[i].StartMs < out[j].StartMs
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

func countDamagedSegments(ranges []Range) int {
	idx := map[int]bool{}
	for _, r := range ranges {
		idx[r.Source.SegmentIndex] = true
	}
	return len(idx)
}

func findSegment(rep manifest.Representation, index int) *manifest.Segment {
	for i := range rep.Segments {
		if rep.Segments[i].Index == index {
			return &rep.Segments[i]
		}
	}
	return nil
}

func rangeKey(segIdx int, kind core.FaultKind) string {
	return string(kind) + "@" + itoa(segIdx)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
