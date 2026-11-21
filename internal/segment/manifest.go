package segment

import (
	"errors"
	"sort"

	"mediacheck/internal/clock"
	"mediacheck/internal/model"
)

// AlignmentError records a representation disagreement resolved by the
// bounded fallback, expressed both in ticks and milliseconds.
type AlignmentError struct {
	Representation string
	CutIndex       int
	DriftTicks     int64
	AnchorTicks    int64
	ObservedTicks  int64
}

// DriftMillis renders the drift for terminal reporting.
func (e AlignmentError) DriftMillis() int64 { return clock.TicksToMilliseconds(e.DriftTicks) }

// SharedManifestInput is the aligned grid handed to manifest encoding.
type SharedManifestInput struct {
	TargetTicks     int64
	Representations []string
	Cuts            []Cut
	Plans           []TrackPlan
}

// Segment is one manifest entry on the shared timeline. All representations
// share identical Start/End ticks; per-representation provenance differs.
type Segment struct {
	Index       int                    `json:"index"`
	Start       int64                  `json:"start_ticks"`
	End         int64                  `json:"end_ticks"`
	State       model.SegmentState     `json:"state"`
	Sources     map[string]model.SourceRef `json:"sources"`
	Gap         *model.TimeRange       `json:"gap,omitempty"`
	Quarantine  *model.TimeRange       `json:"quarantine,omitempty"`
}

// Manifest is the byte-stable, reproducible output of one batch.
type Manifest struct {
	Version             int             `json:"version"`
	InputDigest         string          `json:"input_digest"`
	Representations     []string        `json:"representations"`
	TargetSegmentTicks  int64           `json:"target_segment_ticks"`
	Segments            []Segment       `json:"segments"`
	AlignmentErrors     []AlignmentError `json:"alignment_errors"`
	QuarantinedRanges   []model.TimeRange `json:"quarantined_ranges"`
	RTPStats            *RTPStatsView    `json:"rtp_stats,omitempty"`
}

// RTPStatsView is the RTP recovery summary embedded in the manifest.
type RTPStatsView struct {
	Sent              int     `json:"sent"`
	Received          int     `json:"received"`
	Recovered         int     `json:"recovered"`
	Unrecoverable     int     `json:"unrecoverable"`
	RecoveryPercent   float64 `json:"recovery_percent"`
	MaxTimelineErrMs  int64   `json:"max_timeline_error_ms"`
}

// BuildSegments converts aligned cuts into segments spanning [cut, next).
// Gaps and quarantined suffixes are carried explicitly rather than dropped.
func BuildSegments(in SharedManifestInput, quarantined []model.TimeRange) []Segment {
	n := len(in.Cuts)
	segs := make([]Segment, 0, n)
	for i, c := range in.Cuts {
		end := in.TargetTicks * int64(i+1)
		if i+1 < n {
			end = in.Cuts[i+1].ChosenAt
		}
		seg := Segment{
			Index: i, Start: c.ChosenAt, End: end,
			State:   model.StatePlanned,
			Sources: map[string]model.SourceRef{},
		}
		if c.HasGap {
			seg.State = model.StateQuarantined
			gap := c.Gap
			seg.Gap = &gap
		}
		segs = append(segs, seg)
	}
	for i := range quarantined {
		q := quarantined[i]
		if len(segs) > 0 {
			segs[len(segs)-1].Quarantine = &q
		}
	}
	return segs
}

// ValidateAlignment asserts every representation references the same number
// of cuts and that no residual drift exceeds the configured allowance.
func ValidateAlignment(in SharedManifestInput, maxDrift int64) error {
	if len(in.Plans) == 0 {
		return errors.New("segment: no representation plans to align")
	}
	n := len(in.Cuts)
	for _, tp := range in.Plans {
		if len(tp.Cuts) != n {
			return errors.New("segment: representation cut count mismatch")
		}
	}
	for _, tp := range in.Plans[1:] {
		for i := 0; i < n; i++ {
			d := tp.Cuts[i].ChosenAt - in.Plans[0].Cuts[i].ChosenAt
			if d < 0 {
				d = -d
			}
			if d > maxDrift {
				return errors.New("segment: residual alignment drift exceeds allowance")
			}
		}
	}
	return nil
}

// FinalizeManifest assembles the versioned, deterministically ordered
// manifest. Representation names and segments are sorted for byte stability.
func FinalizeManifest(digest string, in SharedManifestInput, segs []Segment,
	alignErrs []AlignmentError, quarantined []model.TimeRange, rtp *RTPStatsView) *Manifest {
	committed := 0
	for i := range segs {
		if segs[i].State != model.StateQuarantined {
			segs[i].State = model.StateCommitted
			committed++
		}
	}
	reps := append([]string(nil), in.Representations...)
	sort.Strings(reps)
	m := &Manifest{
		Version:            manifestVersion,
		InputDigest:        digest,
		Representations:    reps,
		TargetSegmentTicks: in.TargetTicks,
		Segments:           segs,
		AlignmentErrors:    alignErrs,
		QuarantinedRanges:  quarantined,
		RTPStats:           rtp,
	}
	return m
}

const manifestVersion = 1
