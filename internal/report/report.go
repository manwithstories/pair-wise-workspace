// Package report renders probe, repair and fanout results as a stable,
// machine-readable summary. The JSON shape and the text summary both derive
// from the same struct, so a rerun over unchanged input yields identical
// bytes (no maps, no timestamps, no path-dependent ordering).
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"

	"mediacheck/internal/fanout"
	"mediacheck/internal/fault"
	"mediacheck/internal/probe"
	"mediacheck/internal/repair"
)

// ExitCode maps an outcome to the CLI contract: 0 complete, 2 partial with a
// usable summary, 1 argument or unrecoverable failure.
const (
	ExitOK        = 0
	ExitFailure   = 1
	ExitRecovered = 2
)

// TrackSummary is the per-track block of the report.
type TrackSummary struct {
	ID              uint32          `json:"id"`
	Kind            probe.Kind      `json:"kind"`
	Codec           string          `json:"codec"`
	Timescale       uint32          `json:"timescale"`
	Status          string          `json:"status"`
	Declared        uint64          `json:"declared_samples"`
	Walked          uint64          `json:"walked_samples"`
	Recovered       int             `json:"recovered_samples"`
	Excluded        uint64          `json:"excluded_samples"`
	Keyframes       int             `json:"keyframes"`
	KeyframeGapMS   int64           `json:"keyframe_gap_ms"`
	MeanGapMS       int64           `json:"mean_keyframe_gap_ms"`
	MinGapMS        int64           `json:"min_keyframe_gap_ms"`
	RecoveredMS     int64           `json:"recovered_ms"`
	StartMS         int64           `json:"start_ms"`
	EndMS           int64           `json:"end_ms"`
	DiscardedBytes  int64           `json:"discarded_bytes"`
	FirstUnreadable int64           `json:"first_unreadable_offset"`
	IndexComplete   bool            `json:"index_complete"`
	Unrecoverable   []repair.Span   `json:"unrecoverable"`
	Damages         []repair.Damage `json:"damages"`
}

// SegmentSummary is the manifest segment view.
type SegmentSummary struct {
	Index      int     `json:"index"`
	StartMS    int64   `json:"start_ms"`
	DurationMS int64   `json:"duration_ms"`
	Bytes      int64   `json:"bytes"`
	Samples    int     `json:"samples"`
	Keyframes  int     `json:"keyframes"`
	ActualBPS  float64 `json:"actual_bps"`
	DeltaPct   float64 `json:"bitrate_delta_pct"`
}

// RenditionSummary is one ladder rung in the report.
type RenditionSummary struct {
	Name         string           `json:"name"`
	TargetBPS    int64            `json:"target_bps"`
	ActualBPS    float64          `json:"actual_bps"`
	DeltaPct     float64          `json:"bitrate_delta_pct"`
	QualityValid int              `json:"quality_valid_samples"`
	Segments     []SegmentSummary `json:"segments"`
}

// BoundarySummary is the cross-layer verdict.
type BoundarySummary struct {
	Accepted    bool                  `json:"accepted"`
	ToleranceMS int64                 `json:"tolerance_ms"`
	AlignmentMS int64                 `json:"alignment_ms"`
	MaxDeltaMS  int64                 `json:"max_delta_ms"`
	Edges       []repair.BoundaryEdge `json:"edges"`
	Reasons     []string              `json:"reasons,omitempty"`
}

// Summary is the complete report document.
type Summary struct {
	Format           string           `json:"format"`
	Path             string           `json:"path"`
	Status           string           `json:"status"`
	ExitCode         int              `json:"exit_code"`
	Size             int64            `json:"size"`
	Samples          uint64           `json:"samples"`
	Truncated        bool             `json:"truncated"`
	DiscardedBytes   int64            `json:"discarded_bytes"`
	Tracks           []TrackSummary   `json:"tracks"`
	TargetBPS        int64            `json:"target_bps"`
	ActualBPS        float64          `json:"actual_bps"`
	BitrateDelta     float64          `json:"bitrate_delta_pct"`
	QualityValid     int              `json:"quality_valid_samples"`
	AlignmentMS      int64            `json:"manifest_alignment_ms"`
	KeyframeGapMS    int64            `json:"keyframe_gap_ms"`
	DamageOffset     int64            `json:"damage_offset"`
	RecoveredSamples int              `json:"recovered_samples"`
	Manifest         *ManifestBlock   `json:"manifest,omitempty"`
	Boundary         *BoundarySummary `json:"boundary,omitempty"`
	Issues           []IssueSummary   `json:"issues"`
}

// ManifestBlock groups the ladder output.
type ManifestBlock struct {
	SegmentMS  int64              `json:"segment_ms"`
	Switchable bool               `json:"switchable"`
	Renditions []RenditionSummary `json:"renditions"`
	Reasons    []string           `json:"reasons,omitempty"`
}

// IssueSummary is a classified, locatable fault.
type IssueSummary struct {
	Code     fault.Code `json:"code"`
	Severity string     `json:"severity"`
	Op       string     `json:"op,omitempty"`
	TrackID  uint32     `json:"track_id,omitempty"`
	Offset   int64      `json:"offset"`
	Detail   string     `json:"detail,omitempty"`
}

// Build assembles a Summary from the stage results.
func Build(pf *probe.File, res *repair.Result, man *fanout.Manifest, exit int) *Summary {
	s := &Summary{
		Format:         "mediacheck/v1",
		Path:           pf.Path,
		Size:           pf.Size,
		Truncated:      pf.Truncated,
		DiscardedBytes: pf.DiscardedBytes,
		DamageOffset:   -1,
		ExitCode:       exit,
		Status:         string(res.Status),
	}
	for _, tr := range res.Tracks {
		ts := TrackSummary{
			ID:              tr.TrackID,
			Kind:            tr.Kind,
			Codec:           tr.Codec,
			Timescale:       tr.Timescale,
			Status:          string(tr.Status),
			Recovered:       tr.Recovered.Samples,
			Excluded:        tr.ExcludedSamples,
			Keyframes:       tr.Recovered.Keyframes,
			RecoveredMS:     tr.Recovered.EndMS - tr.Recovered.StartMS,
			StartMS:         tr.Recovered.StartMS,
			EndMS:           tr.Recovered.EndMS,
			DiscardedBytes:  tr.DiscardedBytes,
			FirstUnreadable: tr.FirstUnreadable,
			Unrecoverable:   append([]repair.Span{}, tr.Unrecoverable...),
			Damages:         append([]repair.Damage{}, tr.Damages...),
		}
		if p := probeTrack(pf, tr.TrackID); p != nil {
			// Keyframe gaps describe the declared timeline even when a dry run
			// did not perform payload recovery.
			ts.Declared = p.Declared
			ts.Walked = p.Walked
			ts.KeyframeGapMS = p.MaxKeyframeGapMS
			ts.MeanGapMS = p.MeanKeyframeGapMS
			ts.MinGapMS = p.MinKeyframeGapMS
			ts.IndexComplete = p.Index.Complete
		}
		s.Samples += ts.Walked
		if ts.KeyframeGapMS > s.KeyframeGapMS {
			s.KeyframeGapMS = ts.KeyframeGapMS
		}
		s.Tracks = append(s.Tracks, ts)
		if s.DamageOffset < 0 || (tr.FirstUnreadable >= 0 && tr.FirstUnreadable < s.DamageOffset) {
			s.DamageOffset = tr.FirstUnreadable
		}
		s.RecoveredSamples += tr.Recovered.Samples
	}
	if pf.FirstUnreadable >= 0 && (s.DamageOffset < 0 || pf.FirstUnreadable < s.DamageOffset) {
		s.DamageOffset = pf.FirstUnreadable
	}

	s.Boundary = &BoundarySummary{
		Accepted:    res.Boundary.Accept,
		ToleranceMS: res.Boundary.ToleranceMS,
		AlignmentMS: res.Boundary.AlignmentMS,
		MaxDeltaMS:  res.Boundary.MaxDeltaMS,
		Edges:       append([]repair.BoundaryEdge{}, res.Boundary.Edges...),
		Reasons:     append([]string{}, res.Boundary.Reasons...),
	}
	s.AlignmentMS = res.Boundary.AlignmentMS

	if man != nil {
		block := &ManifestBlock{SegmentMS: man.SegmentMS, Switchable: man.Switchable, Reasons: append([]string{}, man.Reasons...)}
		for _, rv := range man.Renditions {
			rs := RenditionSummary{
				Name:         rv.Name,
				TargetBPS:    rv.TargetBPS,
				ActualBPS:    finite(rv.ActualBPS),
				DeltaPct:     finite(rv.DeltaPct),
				QualityValid: rv.QualitySamples,
			}
			for _, sv := range rv.Segments {
				rs.Segments = append(rs.Segments, SegmentSummary{
					Index:      sv.Index,
					StartMS:    sv.StartMS,
					DurationMS: sv.DurationMS,
					Bytes:      sv.Bytes,
					Samples:    sv.Samples,
					Keyframes:  sv.Keyframes,
					ActualBPS:  finite(sv.ActualBPS),
					DeltaPct:   finite(sv.DeltaPct),
				})
			}
			block.Renditions = append(block.Renditions, rs)
		}
		s.Manifest = block
		// Aggregate the ladder figures the acceptance checks read.
		if len(block.Renditions) > 0 {
			top := block.Renditions[len(block.Renditions)-1]
			s.TargetBPS = top.TargetBPS
			s.ActualBPS = top.ActualBPS
			s.BitrateDelta = top.DeltaPct
			s.QualityValid = top.QualityValid
		}
	}

	for _, f := range allIssues(pf, res) {
		s.Issues = append(s.Issues, IssueSummary{
			Code:     f.Code,
			Severity: string(f.Severity),
			Op:       f.Op,
			TrackID:  f.TrackID,
			Offset:   f.Offset,
			Detail:   f.Detail,
		})
	}
	sortIssues(s.Issues)
	return s
}

// Marshal writes the indented JSON document.
func (s *Summary) Marshal(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// Text writes a compact human summary for the terminal.
func (s *Summary) Text(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "mediacheck %s: status=%s exit=%d\n", s.Path, s.Status, s.ExitCode); err != nil {
		return err
	}
	fmt.Fprintf(w, "tracks=%d samples=%d keyframe_gap_ms=%d damage_offset=%d recovered_samples=%d truncated=%v discarded_bytes=%d\n",
		len(s.Tracks), s.Samples, s.KeyframeGapMS, s.DamageOffset, s.RecoveredSamples, s.Truncated, s.DiscardedBytes)
	for _, t := range s.Tracks {
		fmt.Fprintf(w, "  track %d %s %s: declared=%d walked=%d recovered=%d excluded=%d keyframes=%d keyframe_gap_ms=%d index_complete=%v\n",
			t.ID, t.Kind, t.Codec, t.Declared, t.Walked, t.Recovered, t.Excluded, t.Keyframes, t.KeyframeGapMS, t.IndexComplete)
		for _, d := range t.Damages {
			fmt.Fprintf(w, "    damage %s at %d (sample %d): %s\n", d.Code, d.Offset, d.SampleIndex, d.Detail)
		}
	}
	if s.Manifest != nil {
		fmt.Fprintf(w, "manifest: target_bps=%d actual_bps=%.0f bitrate_delta_pct=%.2f quality_valid_samples=%d manifest_alignment_ms=%d alignment_ms=%d switchable=%v\n",
			s.TargetBPS, s.ActualBPS, s.BitrateDelta, s.QualityValid, s.AlignmentMS, s.AlignmentMS, s.Manifest.Switchable)
	}
	if s.Boundary != nil {
		fmt.Fprintf(w, "boundary: accepted=%v tolerance_ms=%d max_delta_ms=%d\n",
			s.Boundary.Accepted, s.Boundary.ToleranceMS, s.Boundary.MaxDeltaMS)
	}
	if s.Manifest != nil {
		for _, r := range s.Manifest.Reasons {
			fmt.Fprintf(w, "  reason: %s\n", r)
		}
	}
	if len(s.Issues) > 0 {
		fmt.Fprintf(w, "issues=%d\n", len(s.Issues))
		for _, is := range s.Issues {
			fmt.Fprintf(w, "  %s %s %s track=%d offset=%d %s\n", is.Severity, is.Code, is.Op, is.TrackID, is.Offset, is.Detail)
		}
	}
	return nil
}

func probeTrack(pf *probe.File, id uint32) *probe.Track {
	for _, t := range pf.Tracks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// allIssues collects classified faults once. The repair result already
// carries the probe-level faults and per-track repair faults, so they are not
// re-scanned here; boundary faults live on the boundary report.
func allIssues(pf *probe.File, res *repair.Result) []*fault.Fault {
	if res != nil {
		// res.Issues already contains the probe-level and repair faults, so
		// re-reading pf.Issues here would double-count them.
		out := append([]*fault.Fault{}, res.Issues...)
		return append(out, res.Boundary.Issues...)
	}
	if pf != nil {
		return append([]*fault.Fault{}, pf.Issues...)
	}
	return nil
}

// sortIssues orders faults deterministically by offset then code.
func sortIssues(list []IssueSummary) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Offset != list[j].Offset {
			return list[i].Offset < list[j].Offset
		}
		if list[i].Code != list[j].Code {
			return list[i].Code < list[j].Code
		}
		return list[i].TrackID < list[j].TrackID
	})
}

// finite replaces non-finite floats with zero so JSON stays valid.
func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// ExitFor maps a repair status to the exit contract.
func ExitFor(res *repair.Result) int {
	if res == nil {
		return ExitFailure
	}
	switch res.Status {
	case repair.ResultComplete:
		return ExitOK
	case repair.ResultPartial:
		return ExitRecovered
	default:
		return ExitFailure
	}
}
