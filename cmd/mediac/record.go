package main

import (
	"mediac/internal/core"
	"mediac/internal/manifest"
	"mediac/internal/quality"
	"mediac/internal/report"
	"mediac/internal/schedule"
)

// faultSegClockReset is the segment where the clock-reset fixture inserts the
// backwards timestamp; it must match internal/fixture.faultSegA.
const faultSegClockReset = 7

func buildRecord(repID string, mf *manifest.Manifest, m quality.Metrics,
	res *schedule.Result) report.Record {
	r := report.Record{
		Case:            "",
		Representations: len(mf.Representations),
		Segments:        len(mf.Representations[0].Segments),
		TargetBPS:       m.TargetBPS,
		ActualBPS:       m.ActualBPS,
		BitrateDeltaPct: m.BitrateDeltaPct,
		ValidSamplePct:  m.ValidSamplePct,
		GOPP95Ms:        m.GOPQuantilesMs[95],
		DamagedRanges:   qualityRanges(m.DamagedRanges),
		AlignmentMaxMs:  m.AlignmentMaxMs,
		RetryReused:     res.RetriedReused,
		FirstBlocked:    res.FirstBlocked,
	}
	_ = repID
	return r
}

func qualityRanges(in []quality.Range) []report.DamagedRangeJSON {
	out := make([]report.DamagedRangeJSON, 0, len(in))
	for _, r := range in {
		out = append(out, report.DamagedRangeJSON{
			StartMs: r.StartMs, EndMs: r.EndMs, Kind: string(r.Kind),
			RepID: r.Source.RepID, Segment: r.Source.SegmentIndex, Line: r.Source.Line,
		})
	}
	report.SortDamagedRanges(out)
	return out
}

func faultsToRanges(faults []core.Fault) []report.DamagedRangeJSON {
	out := make([]report.DamagedRangeJSON, 0, len(faults))
	for _, f := range faults {
		out = append(out, report.DamagedRangeJSON{
			Kind: string(f.Kind), RepID: f.Source.RepID,
			Segment: f.Source.SegmentIndex, Line: f.Source.Line,
		})
	}
	report.SortDamagedRanges(out)
	return out
}

// aggregateRecords merges per-representation records into the single case-level
// record required by the acceptance output. Segment counts and representations
// are shared; numeric metrics use the middle (reference) representation, while
// damaged ranges and blockage are unioned across all reps.
func aggregateRecords(caseName string, recs []report.Record) report.Record {
	if len(recs) == 0 {
		return report.Record{Case: caseName}
	}
	base := recs[len(recs)/2] // middle representation as the headline metrics
	base.Case = caseName
	seen := map[string]bool{}
	for _, r := range recs {
		for _, d := range r.DamagedRanges {
			key := d.RepID + string(rune(d.Segment)) + d.Kind
			if seen[key] {
				continue
			}
			seen[key] = true
			base.DamagedRanges = append(base.DamagedRanges, d)
		}
		if r.FirstBlocked != "" && base.FirstBlocked == "" {
			base.FirstBlocked = r.FirstBlocked
		}
		base.RetryReused += r.RetryReused
	}
	report.SortDamagedRanges(base.DamagedRanges)
	return base
}
