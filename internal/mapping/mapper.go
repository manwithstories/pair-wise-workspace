// Package mapping applies the manifest's track selection rules to demuxed
// packets. It detects duplicate mappings and codec incompatibility, and can
// isolate a single damaged track while keeping the others flowing.
package mapping

import (
	"fmt"
	"sync"

	"mediadeliver/internal/manifest"
	"mediadeliver/internal/model"
)

// CodecCompat reports whether two codec fourccs can share an output slot.
// A slot keeps a single codec; the empty string means "not yet assigned".
func CodecCompat(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	return a == b
}

// Mapper binds manifest mapping rules to live track descriptors.
type Mapper struct {
	mu sync.Mutex

	// slot assignment per segment: segment -> slot index -> mapping.
	slots map[string]map[int]*model.TrackMapping
	// assigned source tracks per segment for duplicate detection.
	assigned map[string]map[string]bool
	// damaged tracks that are isolated but must not abort the whole run.
	damaged map[string]bool // key: materialID:trackID
	// collected non-fatal track errors (isolated single-track damage).
	trackErrors []*model.Error

	codecByTrack map[string]string // materialID:trackID -> codec
	kindByTrack  map[string]model.TrackKind
}

// New builds an empty Mapper.
func New() *Mapper {
	return &Mapper{
		slots:        map[string]map[int]*model.TrackMapping{},
		assigned:     map[string]map[string]bool{},
		damaged:      map[string]bool{},
		codecByTrack: map[string]string{},
		kindByTrack:  map[string]model.TrackKind{},
	}
}

func trackKey(materialID string, trackID uint32) string {
	return fmt.Sprintf("%s:%d", materialID, trackID)
}

// RegisterTrack records a demuxed descriptor so codec/kind can be validated
// against the manifest rules during Build.
func (m *Mapper) RegisterTrack(materialID string, trackID uint32, kind model.TrackKind, codec string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := trackKey(materialID, trackID)
	m.codecByTrack[k] = codec
	m.kindByTrack[k] = kind
}

// Build resolves every MAP rule of a segment into output-slot mappings.
// It returns KindConflict on duplicates and KindInvalidInput on codec/kind
// incompatibility. A damaged source track is recorded but does not abort the
// other tracks when isolateDamaged is true.
func (m *Mapper) Build(seg string, rules []*manifest.MappingRule, isolateDamaged bool) ([]*model.TrackMapping, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]*model.TrackMapping, 0, len(rules))
	slot := 0
	slotCodec := map[model.TrackKind]string{}
	segAssigned, ok := m.assigned[seg]
	if !ok {
		segAssigned = map[string]bool{}
		m.assigned[seg] = segAssigned
	}

	for _, rule := range rules {
		if rule.Segment != seg {
			continue
		}
		key := trackKey(rule.MaterialID, rule.TrackID)

		// Duplicate source-track mapping within the segment.
		if segAssigned[key] {
			return nil, model.At(model.KindConflict, "mapping", "build",
				int64(rule.Line),
				fmt.Sprintf("segment %s maps source %s more than once", seg, key))
		}

		// Validate the registered track kind/codec against the rule.
		gotKind, known := m.kindByTrack[key]
		if known && gotKind != rule.Kind {
			return nil, model.At(model.KindInvalidInput, "mapping", "build",
				int64(rule.Line),
				fmt.Sprintf("rule wants %s but track %s is %s", rule.Kind, key, gotKind))
		}
		codec := m.codecByTrack[key]
		if existing := slotCodec[rule.Kind]; !CodecCompat(existing, codec) {
			return nil, model.At(model.KindInvalidInput, "mapping", "build",
				int64(rule.Line),
				fmt.Sprintf("incompatible codecs for %s slot: %s vs %s",
					rule.Kind, existing, codec))
		}
		slotCodec[rule.Kind] = codec

		// Isolated single-track damage: record and skip, keep other tracks.
		if m.damaged[key] {
			m.trackErrors = append(m.trackErrors, model.At(
				model.KindCorruptMedia, "mapping", "isolate track", int64(rule.Line),
				fmt.Sprintf("track %s damaged; isolated from segment %s", key, seg)))
			continue
		}

		tm := &model.TrackMapping{
			OutputSlot:  slot,
			Kind:        rule.Kind,
			MaterialID:  rule.MaterialID,
			SourceTrack: rule.TrackID,
			Codec:       codec,
		}
		out = append(out, tm)
		segAssigned[key] = true
		if m.slots[seg] == nil {
			m.slots[seg] = map[int]*model.TrackMapping{}
		}
		m.slots[seg][slot] = tm
		slot++
	}
	return out, nil
}

// MarkDamaged flags a source track as isolated-damaged.
func (m *Mapper) MarkDamaged(materialID string, trackID uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.damaged[trackKey(materialID, trackID)] = true
}

// Accept reports whether a packet's source track is mapped into the segment.
func (m *Mapper) Accept(seg, materialID string, trackID uint32) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := trackKey(materialID, trackID)
	if m.damaged[key] {
		return false
	}
	for _, tm := range m.slots[seg] {
		if tm.MaterialID == materialID && tm.SourceTrack == trackID {
			return true
		}
	}
	return false
}

// Route returns the output slot for a packet, or -1 if it is not mapped.
func (m *Mapper) Route(seg, materialID string, trackID uint32) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tm := range m.slots[seg] {
		if tm.MaterialID == materialID && tm.SourceTrack == trackID {
			return tm.OutputSlot
		}
	}
	return -1
}

// TrackErrors returns the isolated (non-fatal) track damage records.
func (m *Mapper) TrackErrors() []*model.Error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]*model.Error, len(m.trackErrors))
	copy(cp, m.trackErrors)
	return cp
}
