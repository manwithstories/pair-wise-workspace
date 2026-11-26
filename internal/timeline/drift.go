package timeline

import (
	"sort"

	"bq/internal/clock"
	"bq/internal/manifest"
	"bq/internal/model"
)

// AnchorConflictToleranceMs is the maximum residual accepted between
// neighbouring anchors after applying the segment drift model.
const AnchorConflictToleranceMs = 20

// Segment models one piecewise-linear drift interval for a stream.
type Segment struct {
	StartSecond   int64   `json:"start_second"`
	EndSecond     int64   `json:"end_second"`
	OffsetMaster  int64   `json:"offset_master"` // master ticks to add for sync
	SlopePpm      int64   `json:"slope_ppm"`     // linear drift, parts per million
	Recoverable   bool    `json:"recoverable"`
}

// DriftReport is the per-stream drift estimation result. Source offsets are
// never mutated: corrections live only in OffsetMaster.
type DriftReport struct {
	StreamID        int       `json:"stream_id"`
	Segments        []Segment `json:"segments"`
	ResidualMaxMs   int64     `json:"residual_max_ms"`
	Unrecoverable   []Range   `json:"unrecoverable_ranges"`
}

// Range marks a timeline span (master ticks) that cannot be synchronized.
type Range struct {
	StartMaster int64 `json:"start_master"`
	EndMaster   int64 `json:"end_master"`
}

// EstimateDrift fits the piecewise drift model per stream. Anchors flagged
// as conflicting, or neighbours whose fitted residual exceeds the tolerance,
// open an unrecoverable range instead of silently adjusting source data.
func EstimateDrift(m *manifest.Manifest, tl *Timeline) ([]DriftReport, []model.Error) {
	reports := make([]DriftReport, 0, len(m.Streams))
	var errs []model.Error

	for i := range m.Streams {
		s := &m.Streams[i]
		anchors := m.AnchorsForStream(s.ID)
		sort.Slice(anchors, func(a, b int) bool { return anchors[a].AtSecond < anchors[b].AtSecond })

		rate, err := clock.NewRate(s.TimebaseNum, s.TimebaseDen)
		if err != nil {
			errs = append(errs, model.NewError(model.StageTimeline, s.ID, model.ErrCorruptManifest,
				model.SeverityFatal, err.Error(), -1, 0, false))
			return nil, errs
		}
		report := DriftReport{StreamID: s.ID}
		if len(anchors) < 2 {
			// No fit possible: single anchor still gives one constant offset.
			if len(anchors) == 1 {
				off, bad := anchorOffset(anchors[0], rate)
				report.Segments = []Segment{{StartSecond: 0, EndSecond: m.DurationS,
					OffsetMaster: off, Recoverable: !bad && !anchors[0].Conflicts}}
				if bad || anchors[0].Conflicts {
					report.Unrecoverable = append(report.Unrecoverable, fullRange(m))
				}
			}
			reports = append(reports, report)
			continue
		}

		for k := 0; k+1 < len(anchors); k++ {
			a0, a1 := anchors[k], anchors[k+1]
			seg := Segment{
				StartSecond: a0.AtSecond,
				EndSecond:   a1.AtSecond,
				Recoverable: true,
			}
			off0, bad0 := anchorOffset(a0, rate)
			off1, bad1 := anchorOffset(a1, rate)
			seg.OffsetMaster = off0
			seg.SlopePpm = slopePPM(off1-off0, a1.AtSecond-a0.AtSecond)
			residual := residualMs(off0, off1)

			if a0.Conflicts || a1.Conflicts || bad0 || bad1 ||
				residual > AnchorConflictToleranceMs {
				seg.Recoverable = false
				startM, _ := clock.MsToMaster(a0.AtSecond * 1000)
				endM, _ := clock.MsToMaster(a1.AtSecond * 1000)
				report.Unrecoverable = append(report.Unrecoverable, Range{StartMaster: startM, EndMaster: endM})
				errs = append(errs, model.NewError(model.StageTimeline, s.ID, model.ErrAnchorConflict,
					model.SeverityError, "anchor conflict or drift beyond tolerance",
					-1, a0.SourcePTS, true))
			} else if residual > report.ResidualMaxMs {
				report.ResidualMaxMs = residual
			}
			report.Segments = append(report.Segments, seg)
		}
		reports = append(reports, report)
	}
	return reports, errs
}

// anchorOffset computes the master-tick correction implied by one anchor:
// where the master clock expects the point minus where the source places it.
func anchorOffset(a manifest.Anchor, rate clock.Rate) (int64, bool) {
	sourceMaster, err := rate.ToMaster(a.SourcePTS)
	if err != nil {
		return 0, true
	}
	return a.MasterTicks - sourceMaster, false
}

func slopePPM(deltaOffsetMaster int64, deltaSeconds int64) int64 {
	if deltaSeconds <= 0 {
		return 0
	}
	// ppm = (offset delta in ticks / master ticks in span) * 1e6
	spanMaster, err := clock.MsToMaster(deltaSeconds * 1000)
	if err != nil || spanMaster == 0 {
		return 0
	}
	return deltaOffsetMaster * 1_000_000 / spanMaster
}

func residualMs(off0, off1 int64) int64 {
	d := off1 - off0
	if d < 0 {
		d = -d
	}
	return clock.MasterToMs(d)
}

func fullRange(m *manifest.Manifest) Range {
	start, _ := clock.MsToMaster(0)
	end, _ := clock.MsToMaster(m.DurationS * 1000)
	return Range{StartMaster: start, EndMaster: end}
}

// SyncOffsetAt returns the drift correction (master ticks) at a timeline
// position for a report, and whether the position lies in a recoverable
// segment. The renderer and verifier both apply this read-only correction.
func (r DriftReport) SyncOffsetAt(masterPTS int64) (int64, bool) {
	sec := masterPTS / clock.MasterHz
	for _, seg := range r.Segments {
		if sec >= seg.StartSecond && sec < seg.EndSecond {
			if !seg.Recoverable {
				return 0, false
			}
			elapsed := masterPTS - seg.StartSecond*clock.MasterHz
			return seg.OffsetMaster + elapsed*seg.SlopePpm/1_000_000, true
		}
	}
	if len(r.Segments) == 0 {
		return 0, true
	}
	last := r.Segments[len(r.Segments)-1]
	return last.OffsetMaster, last.Recoverable
}
