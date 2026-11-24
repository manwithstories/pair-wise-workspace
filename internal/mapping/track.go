// Package mapping assigns capture sources to presentation tracks and enforces
// the one-source/one-track invariants: missing, conflicting or duplicate
// mappings are rejected instead of silently merging streams.
package mapping

import (
	"fmt"
	"sort"

	"relay/internal/media"
)

// Rule binds one capture source to one output track. TrackID is the stable
// identity used inside the remuxed container.
type Rule struct {
	SourceID uint32
	Kind     media.Kind
	TrackID  uint32
}

// Mapper holds a validated, immutable set of mapping rules.
type Mapper struct {
	bySource map[uint32]Rule
}

// NewMapper validates rules up front and rejects duplicates or track slots
// claimed by more than one source.
func NewMapper(rules []Rule) (*Mapper, error) {
	m := &Mapper{bySource: map[uint32]Rule{}}
	trackOwner := map[uint32]uint32{}
	for _, r := range rules {
		if r.SourceID == 0 {
			return nil, media.NewError(media.ErrMapping, r.SourceID, "source id must be non-zero")
		}
		if r.Kind != media.KindVideo && r.Kind != media.KindAudio {
			return nil, media.NewError(media.ErrMapping, r.SourceID, "rule has unknown media kind")
		}
		if _, dup := m.bySource[r.SourceID]; dup {
			return nil, media.NewError(media.ErrMapping, r.SourceID, "duplicate mapping rule for source")
		}
		if owner, taken := trackOwner[r.TrackID]; taken && owner != r.SourceID {
			return nil, media.NewError(media.ErrMapping, r.SourceID,
				fmt.Sprintf("track %d already claimed by source %d", r.TrackID, owner))
		}
		m.bySource[r.SourceID] = r
		trackOwner[r.TrackID] = r.SourceID
	}
	return m, nil
}

// DefaultRules derives one rule per observed source. It fails when one source
// carries samples of more than one kind, since that would be an ambiguous
// mapping rather than a legitimate mixed-content track.
func DefaultRules(frames []*media.RawFrame) ([]Rule, error) {
	kind := map[uint32]media.Kind{}
	for _, fr := range frames {
		if k, seen := kind[fr.Meta.SourceID]; seen && k != fr.Kind {
			return nil, media.NewError(media.ErrMapping, fr.Meta.SourceID,
				"source emits mixed media kinds; explicit mapping required")
		}
		kind[fr.Meta.SourceID] = fr.Kind
	}
	sources := make([]uint32, 0, len(kind))
	for s := range kind {
		sources = append(sources, s)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i] < sources[j] })
	rules := make([]Rule, 0, len(sources))
	for i, s := range sources {
		rules = append(rules, Rule{SourceID: s, Kind: kind[s], TrackID: uint32(i + 1)})
	}
	return rules, nil
}

// Track groups the frames of one output track in presentation order.
type Track struct {
	Rule   Rule
	Frames []*media.RawFrame
}

// Map validates every frame against the rules and groups them by output
// track. It rejects unmapped sources, kind conflicts and duplicate sequence
// numbers within a source.
func (m *Mapper) Map(frames []*media.RawFrame) (map[uint32]*Track, error) {
	out := map[uint32]*Track{}
	seenSeq := map[uint32]map[uint64]bool{}
	for _, fr := range frames {
		rule, ok := m.bySource[fr.Meta.SourceID]
		if !ok {
			return nil, media.NewError(media.ErrMapping, fr.Meta.SourceID,
				"no mapping rule for source")
		}
		if fr.Kind != rule.Kind {
			return nil, media.NewError(media.ErrMapping, fr.Meta.SourceID,
				fmt.Sprintf("frame kind %s conflicts with mapped kind %s", fr.Kind, rule.Kind))
		}
		seqs, ok := seenSeq[fr.Meta.SourceID]
		if !ok {
			seqs = map[uint64]bool{}
			seenSeq[fr.Meta.SourceID] = seqs
		}
		if seqs[fr.Meta.Sequence] {
			return nil, media.NewError(media.ErrMapping, fr.Meta.SourceID,
				fmt.Sprintf("duplicate sequence %d", fr.Meta.Sequence))
		}
		seqs[fr.Meta.Sequence] = true
		tr, ok := out[rule.TrackID]
		if !ok {
			tr = &Track{Rule: rule}
			out[rule.TrackID] = tr
		}
		tr.Frames = append(tr.Frames, fr)
	}
	for _, tr := range out {
		sort.SliceStable(tr.Frames, func(i, j int) bool {
			return tr.Frames[i].Meta.PresentationNS < tr.Frames[j].Meta.PresentationNS
		})
	}
	return out, nil
}

// TrackIDs returns the sorted output track IDs, for deterministic iteration.
func (m *Mapper) TrackIDs() []uint32 {
	seen := map[uint32]bool{}
	for _, r := range m.bySource {
		seen[r.TrackID] = true
	}
	ids := make([]uint32, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
