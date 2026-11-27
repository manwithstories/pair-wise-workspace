// Package analyze summarizes track structure and keyframe cadence from
// demuxed packets on the shared 27 MHz clock.
package analyze

import (
	"tscheck/internal/model"
	"tscheck/internal/ts"
)

// TrackStats summarizes one elementary stream.
type TrackStats struct {
	PID           uint16
	StreamType    uint8
	Packets       int
	PayloadBytes  int64
	FirstPTS      model.Ticks
	LastPTS       model.Ticks
	SeenPTS       bool
	UnanchoredPTS bool // at least one PTS had no trusted clock anchor
}

// Structure aggregates per-PID statistics during a scan.
type Structure struct {
	tracks  map[uint16]*TrackStats
	types   map[uint16]uint8
	Faults  []model.Fault
}

// NewStructure creates an empty Structure.
func NewStructure() *Structure {
	return &Structure{tracks: map[uint16]*TrackStats{}, types: map[uint16]uint8{}}
}

// SetStreams records the trusted PSI stream mapping.
func (s *Structure) SetStreams(streams []ts.StreamInfo) {
	for _, st := range streams {
		s.types[st.PID] = st.StreamType
	}
}

// AddPacket accounts one demuxed packet.
func (s *Structure) AddPacket(pi ts.PacketInfo) {
	tr := s.tracks[pi.PID]
	if tr == nil {
		tr = &TrackStats{PID: pi.PID, StreamType: s.types[pi.PID]}
		s.tracks[pi.PID] = tr
	}
	tr.Packets++
	tr.PayloadBytes += int64(len(pi.Payload))
}

// AddPTS records an unwrapped presentation timestamp for a track.
func (s *Structure) AddPTS(pid uint16, ts model.Timestamp) {
	tr := s.tracks[pid]
	if tr == nil {
		tr = &TrackStats{PID: pid, StreamType: s.types[pid]}
		s.tracks[pid] = tr
	}
	if !ts.Anchored {
		tr.UnanchoredPTS = true
		return
	}
	if !tr.SeenPTS || ts.Ticks < tr.FirstPTS {
		tr.FirstPTS = ts.Ticks
	}
	if !tr.SeenPTS || ts.Ticks > tr.LastPTS {
		tr.LastPTS = ts.Ticks
	}
	tr.SeenPTS = true
}

// Report returns per-track stats ordered by PID.
func (s *Structure) Report() []TrackStats {
	out := make([]TrackStats, 0, len(s.tracks))
	for _, tr := range s.tracks {
		out = append(out, *tr)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].PID < out[j-1].PID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
