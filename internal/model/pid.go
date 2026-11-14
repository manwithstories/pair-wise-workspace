package model

import "tsaudit/internal/ts"

// PIDStats is the packet-level snapshot for one PID.
type PIDStats struct {
	PID          uint16 `json:"pid"`
	Packets      int64  `json:"packets"`
	PayloadBytes int64  `json:"payload_bytes"`
	FirstPacket  int64  `json:"first_pkt"`
	LastPacket   int64  `json:"last_pkt"`
}

// RecordPacket updates the PID snapshot from a validated TS packet.
func (s *State) RecordPacket(p *ts.Packet) {
	if p.Header.PID == ts.NullPID {
		s.NullPkts++
		return
	}
	st := s.PIDStats[p.Header.PID]
	if st == nil {
		st = &PIDStats{PID: p.Header.PID, FirstPacket: p.Index}
		s.PIDStats[p.Header.PID] = st
	}
	st.Packets++
	st.PayloadBytes += int64(len(p.Payload))
	st.LastPacket = p.Index
}

// OrphanPMTPrograms returns PAT programs for which no PMT section was ever seen.
func (s *State) OrphanPMTPrograms() []uint16 {
	var out []uint16
	for _, pr := range s.PAT.Programs {
		if pr.IsNIT {
			continue
		}
		if _, ok := s.PMTs[pr.ProgramNumber]; !ok {
			out = append(out, pr.ProgramNumber)
		}
	}
	return out
}
