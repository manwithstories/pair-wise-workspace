package core

import (
	"sort"
	"strings"

	"media-check/internal/model"
)

// SegmentKind classifies a segment in the recovery plan.
type SegmentKind string

const (
	SegmentValid   SegmentKind = "valid"
	SegmentCorrupt SegmentKind = "corrupt"
)

// SegmentState is one segment's planned disposition.
type SegmentState struct {
	Index   int
	Range   model.TimeRange
	Kind    SegmentKind
	Reasons []string
}

// RecoveryPlan divides an asset into valid and corrupt ranges.
type RecoveryPlan struct {
	Segments      []SegmentState
	CorruptRanges int
}

// CorruptFaultCodes are adapter/core faults that make a segment corrupt
// for recovery purposes. Metadata-only findings (colour carry-forward,
// audio isolation) are tracked separately and do not corrupt a segment.
var CorruptFaultCodes = map[model.FaultCode]bool{
	model.FaultSeqDuplicate:        true,
	model.FaultSeqGap:              true,
	model.FaultHashMismatch:        true,
	model.FaultTimestampRegression: true,
	model.FaultBoxTruncated:        true,
	model.FaultSampleOffset:        true,
	model.FaultNoIndex:             true,
}

// BuildPlan derives the recovery plan from located faults. Segment time
// ranges come from the inspector's unified time base so index and repair
// never disagree.
func BuildPlan(segRanges []model.TimeRange, faults model.Faults) RecoveryPlan {
	plan := RecoveryPlan{Segments: make([]SegmentState, len(segRanges))}
	reasons := map[int][]string{}
	for _, f := range faults {
		if f.Segment < 0 || f.Segment >= len(segRanges) || !CorruptFaultCodes[f.Code] {
			continue
		}
		reasons[f.Segment] = append(reasons[f.Segment], string(f.Code)+": "+f.Detail)
	}
	for i, r := range segRanges {
		st := SegmentState{Index: i, Range: r, Kind: SegmentValid}
		if rs, bad := reasons[i]; bad {
			st.Kind = SegmentCorrupt
			sort.Strings(rs)
			st.Reasons = rs
			plan.CorruptRanges++
		}
		plan.Segments[i] = st
	}
	return plan
}

// Summary renders the plan as one compact line per segment.
func (p RecoveryPlan) Summary() []string {
	out := make([]string, 0, len(p.Segments))
	for _, s := range p.Segments {
		line := string(s.Kind)
		if len(s.Reasons) > 0 {
			line += " (" + strings.Join(s.Reasons, "; ") + ")"
		}
		out = append(out, line)
	}
	return out
}
