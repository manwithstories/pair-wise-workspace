// Package mapping assigns output tracks from init-segment track
// information. Tracks are identified by codec, language and channel
// layout; tracks that share an identity are never merged.
package mapping

import (
	"fmt"

	"relaycheck/internal/clock"
	"relaycheck/internal/demux"
)

// Key is the mapping identity of a track: codec, language, channel layout
// and handler type.
type Key struct {
	Codec    string
	Language string
	Channels uint16
	Handler  string
}

// KeyOf derives the mapping identity of a track.
func KeyOf(ti demux.TrackInfo) Key {
	return Key{Codec: ti.Codec, Language: ti.Language, Channels: ti.Channels, Handler: ti.Handler}
}

// Track is one mapped output track.
type Track struct {
	Info  demux.TrackInfo
	OutID uint32
}

// Mapper holds the track mapping derived from an init segment.
type Mapper struct {
	byID   map[uint32]*Track
	groups map[Key][]uint32
	order  []uint32
}

// NewMapper builds the mapping for an init segment. Identity conflicts are
// reported as informational faults; the conflicting tracks are kept as
// separate output tracks and never merged.
func NewMapper(init *demux.InitInfo) (*Mapper, []clock.Fault) {
	m := &Mapper{byID: map[uint32]*Track{}, groups: map[Key][]uint32{}}
	var faults []clock.Fault
	for _, ti := range init.Tracks {
		if ti.ID == 0 {
			faults = append(faults, clock.NewFault(clock.FaultMapConflict, 0, 0, -1,
				"track id 0 is invalid"))
			continue
		}
		if _, dup := m.byID[ti.ID]; dup {
			faults = append(faults, clock.NewFault(clock.FaultMapConflict, 0, ti.ID, -1,
				"duplicate track id in init"))
			continue
		}
		k := KeyOf(ti)
		if ids := m.groups[k]; len(ids) > 0 {
			faults = append(faults, clock.NewFault(clock.FaultMapConflict, 0, ti.ID, -1,
				fmt.Sprintf("identity shared with track %d; kept separate", ids[0])))
		}
		m.groups[k] = append(m.groups[k], ti.ID)
		m.byID[ti.ID] = &Track{Info: ti, OutID: ti.ID}
		m.order = append(m.order, ti.ID)
	}
	return m, faults
}

// Lookup resolves an input track id. Unknown ids are map conflicts.
func (m *Mapper) Lookup(id uint32) (*Track, *clock.Fault) {
	if t, ok := m.byID[id]; ok {
		return t, nil
	}
	f := clock.NewFault(clock.FaultMapConflict, 0, id, -1, "track not present in init segment")
	return nil, &f
}

// Tracks returns the mapped tracks in init order.
func (m *Mapper) Tracks() []*Track {
	out := make([]*Track, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.byID[id])
	}
	return out
}
