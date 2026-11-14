// Package report builds and serializes the terminal audit report.
package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"tsaudit/internal/audit"
	"tsaudit/internal/model"
	"tsaudit/internal/psi"
)

// PATReport is the final PAT view.
type PATReport struct {
	PID               uint16              `json:"pid"`
	TransportStreamID uint16              `json:"transport_stream_id"`
	Version           uint8               `json:"version"`
	CurrentNext       bool                `json:"current_next"`
	VersionChanges    int                 `json:"version_changes"`
	Versions          []model.VersionInfo `json:"versions"`
	Programs          []psi.PATProgram    `json:"programs"`
}

// PMTReport is the final per-program PMT view.
type PMTReport struct {
	PID            uint16                 `json:"pid"`
	PMTPIDs        []uint16               `json:"pmt_pids"`
	Program        uint16                 `json:"program"`
	Version        uint8                  `json:"version"`
	CurrentNext    bool                   `json:"current_next"`
	VersionChanges int                    `json:"version_changes"`
	Versions       []model.VersionInfo    `json:"versions"`
	PCRPID         uint16                 `json:"pcr_pid"`
	ProgramInfo    []psi.Descriptor       `json:"program_info,omitempty"`
	Streams        []psi.ElementaryStream `json:"streams"`
}

// PIDMapEntry is packet-level evidence for one PID.
type PIDMapEntry struct {
	PID          uint16   `json:"pid"`
	Program      uint16   `json:"program,omitempty"`
	StreamType   uint8    `json:"stream_type,omitempty"`
	Language     string   `json:"language,omitempty"`
	Registration []string `json:"registration,omitempty"`
	Packets      int64    `json:"packets"`
	PayloadBytes int64    `json:"payload_bytes"`
	FirstPacket  int64    `json:"first_pkt"`
	LastPacket   int64    `json:"last_pkt"`
	Declared     bool     `json:"declared"`
	Orphan       bool     `json:"orphan"`
}

// OrphanPMT identifies a PAT reference that never produced a PMT.
type OrphanPMT struct {
	Program uint16 `json:"program"`
	PID     uint16 `json:"pid"`
}

// Report is the stable JSON contract consumed by verify and downstream tools.
type Report struct {
	File              string                `json:"file"`
	BytesTotal        int64                 `json:"bytes_total"`
	PacketSize        int                   `json:"packet_size"`
	PacketsTotal      int64                 `json:"packets_total"`
	SyncLosses        int64                 `json:"sync_losses"`
	GarbageBytes      int64                 `json:"garbage_bytes"`
	CRCErrors         int64                 `json:"crc_errors"`
	DiscardedSections int64                 `json:"discarded_sections"`
	UnclaimedPSI      int64                 `json:"unclaimed_psi_packets"`
	PAT               *PATReport            `json:"pat,omitempty"`
	PMTs              []PMTReport           `json:"pmt"`
	PIDMap            []PIDMapEntry         `json:"pid_map"`
	OrphanPMTs        []OrphanPMT           `json:"orphan_pmts"`
	OrphanPIDs        []uint16              `json:"orphan_pids"`
	ZeroPacketES      []PIDMapEntry         `json:"zero_packet_es"`
	Continuity        audit.ContinuityStats `json:"continuity"`
	PCR               []audit.PCRStats      `json:"pcr"`
	NullPackets       int64                 `json:"null_packets"`
}

func buildPAT(t *model.PATTracker) *PATReport {
	if t == nil || t.Current == nil {
		return nil
	}
	return &PATReport{
		PID:               0,
		TransportStreamID: t.TransportStreamID,
		Version:           t.CurrentVersion,
		CurrentNext:       t.CurrentNext,
		VersionChanges:    t.VersionChanges,
		Versions:          append([]model.VersionInfo(nil), t.Versions...),
		Programs:          append([]psi.PATProgram(nil), t.Programs...),
	}
}

func buildPMTs(state *model.State) []PMTReport {
	trackers := state.PMTTrackers()
	out := make([]PMTReport, 0, len(trackers))
	for _, t := range trackers {
		if t.Current == nil {
			continue
		}
		out = append(out, PMTReport{
			PID:            t.CurrentPMTPID,
			PMTPIDs:        append([]uint16(nil), t.PMTPIDs...),
			Program:        t.ProgramNumber,
			Version:        t.CurrentVersion,
			CurrentNext:    t.CurrentNext,
			VersionChanges: t.VersionChanges,
			Versions:       append([]model.VersionInfo(nil), t.Versions...),
			PCRPID:         t.Current.PCRPID,
			ProgramInfo:    append([]psi.Descriptor(nil), t.Current.ProgramInfo...),
			Streams:        append([]psi.ElementaryStream(nil), t.Current.Streams...),
		})
	}
	return out
}

func buildPIDMap(state *model.State) ([]PIDMapEntry, []uint16, []PIDMapEntry) {
	decl := state.Declarations()
	active := state.ActiveDeclarations()
	entries := make([]PIDMapEntry, 0)
	orphans := make([]uint16, 0)
	seen := make(map[uint16]bool)
	pids := make([]uint16, 0, len(state.PIDStats))
	for pid := range state.PIDStats {
		pids = append(pids, pid)
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	for _, pid := range pids {
		st := state.PIDStats[pid]
		d, declared := decl[pid]
		if declared && d.Kind != "ES" {
			continue
		}
		e := PIDMapEntry{
			PID: pid, Packets: st.Packets, PayloadBytes: st.PayloadBytes,
			FirstPacket: st.FirstPacket, LastPacket: st.LastPacket, Declared: declared,
		}
		if declared {
			e.Program, e.StreamType, e.Language, e.Registration = d.Program, d.StreamType, d.Language, d.Registration
		} else {
			e.Orphan = true
			orphans = append(orphans, pid)
		}
		entries = append(entries, e)
		seen[pid] = true
	}
	zero := make([]PIDMapEntry, 0)
	for pid, d := range active {
		if d.Kind != "ES" || seen[pid] {
			continue
		}
		e := PIDMapEntry{PID: pid, Program: d.Program, StreamType: d.StreamType, Language: d.Language,
			Registration: d.Registration, FirstPacket: -1, LastPacket: -1, Declared: true}
		entries = append(entries, e)
		zero = append(zero, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].PID < entries[j].PID })
	sort.Slice(zero, func(i, j int) bool { return zero[i].PID < zero[j].PID })
	sort.Slice(orphans, func(i, j int) bool { return orphans[i] < orphans[j] })
	return entries, orphans, zero
}

func orphanPMTs(state *model.State) []OrphanPMT {
	progs := state.OrphanPMTPrograms()
	out := make([]OrphanPMT, 0, len(progs))
	for _, pr := range state.PAT.Programs {
		for _, p := range progs {
			if pr.ProgramNumber == p {
				out = append(out, OrphanPMT{Program: p, PID: pr.PID})
			}
		}
	}
	return out
}

// WriteJSON creates parent directories and writes an indented JSON report.
func (r *Report) WriteJSON(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}
