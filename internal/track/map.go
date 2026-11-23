// Package track maps source tracks onto a stable output numbering using a
// user-supplied rule set. Mapped track numbers never change once assigned, a
// missing source track aborts before any partial summary is produced, and
// ignored tracks are dropped deterministically.
package track

import (
	"sort"

	"mediacore/internal/fault"
	"mediacore/internal/sample"
)

// Action describes what the mapping does with a source track.
type Action string

const (
	ActionKeep   Action = "keep"   // emit under MappedNumber, optionally renamed
	ActionIgnore Action = "ignore" // drop the track entirely
)

// Rule is one user-supplied mapping entry keyed by source track index.
type Rule struct {
	SourceIndex  int
	Action       Action
	MappedNumber uint32 // required for keep; stable output number
	Rename       sample.Codec
}

// Mapping is the resolved, validated set of rules.
type Mapping struct {
	bySource map[int]Rule
	// Order is the sorted output sequence of mapped numbers.
	Order []uint32
	// numberToSource maps an output number back to its source index.
	numberToSource map[uint32]int
	// codecOverride carries optional renames per output number.
	codecOverride map[uint32]sample.Codec
}

// Build validates rules against the available source count and returns a
// ready mapping. Any reference to a missing source track, duplicate output
// number, or invalid rule aborts with an offset-less configuration fault.
func Build(rules []Rule, sourceCount int) (*Mapping, error) {
	m := &Mapping{
		bySource:       map[int]Rule{},
		numberToSource: map[uint32]int{},
		codecOverride:  map[uint32]sample.Codec{},
	}
	for _, rl := range rules {
		if rl.SourceIndex < 0 || rl.SourceIndex >= sourceCount {
			return nil, fault.New(fault.KindLimitExceeded, fault.Abort, -1,
				"mapping references missing source track").
				WithTrack(0, rl.SourceIndex)
		}
		if _, dup := m.bySource[rl.SourceIndex]; dup {
			return nil, fault.New(fault.KindLimitExceeded, fault.Abort, -1,
				"duplicate mapping rule for source track").
				WithTrack(0, rl.SourceIndex)
		}
		if rl.Action == ActionKeep {
			if rl.MappedNumber == 0 {
				return nil, fault.New(fault.KindLimitExceeded, fault.Abort, -1,
					"keep rule requires a non-zero mapped number").
					WithTrack(0, rl.SourceIndex)
			}
			if other, clash := m.numberToSource[rl.MappedNumber]; clash {
				return nil, fault.New(fault.KindLimitExceeded, fault.Abort, -1,
					"mapped number collision").WithTrack(0, other)
			}
			m.numberToSource[rl.MappedNumber] = rl.SourceIndex
			if rl.Rename != "" {
				m.codecOverride[rl.MappedNumber] = rl.Rename
			}
			m.Order = append(m.Order, rl.MappedNumber)
		} else if rl.Action != ActionIgnore {
			return nil, fault.New(fault.KindLimitExceeded, fault.Abort, -1,
				"unknown mapping action").WithTrack(0, rl.SourceIndex)
		}
		m.bySource[rl.SourceIndex] = rl
	}
	sort.Slice(m.Order, func(i, j int) bool { return m.Order[i] < m.Order[j] })
	return m, nil
}

// DefaultMapping keeps every source track with 1-based stable numbers.
func DefaultMapping(sourceCount int) *Mapping {
	rules := make([]Rule, sourceCount)
	for i := 0; i < sourceCount; i++ {
		rules[i] = Rule{SourceIndex: i, Action: ActionKeep, MappedNumber: uint32(i + 1)}
	}
	mp, err := Build(rules, sourceCount)
	if err != nil {
		panic(err)
	}
	return mp
}

// Resolved is a source track's place in the output.
type Resolved struct {
	MappedNumber uint32
	Codec        sample.Codec
	Ignored      bool
}

// Resolve returns the output placement for a source track index. A source
// without an explicit rule is ignored by default.
func (m *Mapping) Resolve(sourceIndex int, original sample.Codec) Resolved {
	rl, ok := m.bySource[sourceIndex]
	if !ok || rl.Action == ActionIgnore {
		return Resolved{Ignored: true}
	}
	codec := original
	if renamed, has := m.codecOverride[rl.MappedNumber]; has {
		codec = renamed
	}
	return Resolved{MappedNumber: rl.MappedNumber, Codec: codec}
}

// SourceFor returns the source index behind an output number, and whether it
// exists.
func (m *Mapping) SourceFor(mappedNumber uint32) (int, bool) {
	src, ok := m.numberToSource[mappedNumber]
	return src, ok
}

// Apply rewrites each sample's mapped track number and codec, dropping ignored
// tracks. Output ordering is unchanged; deterministic merge happens later.
func (m *Mapping) Apply(samples []*sample.Sample) []*sample.Sample {
	out := make([]*sample.Sample, 0, len(samples))
	for _, s := range samples {
		rl, ok := m.bySource[s.TrackIndex]
		if !ok || rl.Action == ActionIgnore {
			continue
		}
		cp := *s
		cp.TrackID = rl.MappedNumber
		if renamed, has := m.codecOverride[rl.MappedNumber]; has {
			cp.Codec = renamed
		}
		out = append(out, &cp)
	}
	return out
}
