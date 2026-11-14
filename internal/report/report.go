// Package report scans a TS stream and produces JSON/text reports.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"tsprobe/internal/cc"
	"tsprobe/internal/psi"
	"tsprobe/internal/tspkt"
)

// PIDInfo is one row of the PID mapping table.
type PIDInfo struct {
	PID        uint16 `json:"pid"`
	Role       string `json:"role"`
	Program    uint16 `json:"program,omitempty"`
	StreamType string `json:"stream_type,omitempty"`
	Packets    uint64 `json:"packets"`
	Bytes      uint64 `json:"bytes"`
}

// ProgramSummary is the per-program part of the report.
type ProgramSummary struct {
	ProgramNumber uint16           `json:"program_number"`
	PMTPID        uint16           `json:"pmt_pid"`
	PCRPID        uint16           `json:"pcr_pid,omitempty"`
	PMTVersion    int              `json:"pmt_version"`
	Versions      psi.VersionStats `json:"versions"`
	Streams       []psi.Stream     `json:"streams"`
	Descriptors   []psi.Descriptor `json:"descriptors,omitempty"`
}

// Report is the full scan result.
type Report struct {
	File         string `json:"file"`
	PacketSize   int    `json:"packet_size"`
	Packets      uint64 `json:"packets"`
	Bytes        uint64 `json:"bytes"`
	BadSync      uint64 `json:"bad_sync"`
	Malformed    uint64 `json:"malformed"`
	CRCErrors    uint64 `json:"crc_errors"`
	LengthErrors uint64 `json:"length_errors"`

	PATFound          bool             `json:"pat_found"`
	PATFirstPacket    int64            `json:"pat_first_packet"`
	PATVersion        int              `json:"pat_version"`
	PATVersionStats   psi.VersionStats `json:"pat_version_stats"`
	PATVersionChanges uint64           `json:"pat_version_changes"`
	NITPIDs           []uint16         `json:"nit_pids,omitempty"`

	Programs []ProgramSummary `json:"programs"`

	CCJumpEvents   uint64     `json:"cc_jump_events"`
	LossEvents     uint64     `json:"loss_events"`
	LostPacketsEst uint64     `json:"lost_packets_est"`
	Duplicates     uint64     `json:"duplicates"`
	CCEvents       []cc.Event `json:"cc_events"`

	NullPackets uint64 `json:"null_packets"`
	ESPackets   uint64 `json:"es_packets"`
	PSIPackets  uint64 `json:"psi_packets"`

	PIDs []PIDInfo `json:"pids"`
}

type pidCount struct {
	packets uint64
	bytes   uint64
}

// Scan reads the whole stream and builds a Report.
func Scan(src io.Reader, name string) (*Report, error) {
	rd, err := tspkt.NewReader(src)
	if err != nil {
		return nil, err
	}
	reg := psi.NewRegistry()
	trk := cc.New()
	var psiCnt psi.Counters
	counts := map[uint16]*pidCount{}
	assemblers := map[uint16]*psi.Assembler{}
	getAsm := func(pid uint16) *psi.Assembler {
		a := assemblers[pid]
		if a == nil {
			a = psi.NewAssembler(pid)
			assemblers[pid] = a
		}
		return a
	}
	rep := &Report{File: name, PATFirstPacket: -1}
	var total uint64
	for {
		pkt, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		total++
		c := counts[pkt.PID]
		if c == nil {
			c = &pidCount{}
			counts[pkt.PID] = c
		}
		c.packets++
		c.bytes += tspkt.PacketSize
		if pkt.PID != 0x1FFF {
			trk.Observe(pkt.PID, pkt.ContinuityCounter, pkt.DiscontinuityIndicator,
				pkt.HasPayload, pkt.Payload, pkt.Offset)
		}
		if !pkt.HasPayload {
			continue
		}
		switch {
		case pkt.PID == 0x0000:
			for _, sec := range getAsm(0).Feed(pkt.Payload, pkt.PayloadUnitStart, pkt.Offset, &psiCnt) {
				reg.ApplyPATSection(sec.Raw)
				if rep.PATFirstPacket < 0 && reg.PAT != nil {
					rep.PATFirstPacket = pkt.Index
				}
			}
		case reg.IsPMTPID(pkt.PID):
			for _, sec := range getAsm(pkt.PID).Feed(pkt.Payload, pkt.PayloadUnitStart, pkt.Offset, &psiCnt) {
				reg.ApplyPMTSection(sec.Raw)
			}
		}
	}
	rep.PacketSize = rd.PacketSize()
	rep.Packets = total
	rep.Bytes = total * tspkt.PacketSize
	rep.BadSync = rd.BadSync
	rep.Malformed = rd.Malformed
	rep.CRCErrors = psiCnt.CRCErrors
	rep.LengthErrors = psiCnt.LengthErrors
	rep.PATFound = reg.PAT != nil
	if reg.PAT != nil {
		rep.PATVersion = int(reg.PAT.Version)
		rep.NITPIDs = reg.PAT.NITPIDs
	}
	rep.PATVersionStats = reg.PATStats
	rep.PATVersionChanges = reg.PATStats.Changes
	rep.CCJumpEvents = trk.JumpEvents
	rep.LossEvents = trk.LossEvents
	rep.LostPacketsEst = trk.LostEstimate
	rep.Duplicates = trk.Duplicates
	rep.CCEvents = trk.Events
	for _, p := range reg.Programs() {
		ps := ProgramSummary{
			ProgramNumber: p.ProgramNumber,
			PMTPID:        p.PMTPID,
			PMTVersion:    -1,
			Versions:      p.Stats,
		}
		if p.PMT != nil {
			ps.PCRPID = p.PMT.PCRPID
			ps.PMTVersion = int(p.PMT.Version)
			ps.Streams = p.PMT.Streams
			ps.Descriptors = p.PMT.Descriptors
		}
		rep.Programs = append(rep.Programs, ps)
	}
	rep.buildPIDTable(reg, counts)
	return rep, nil
}

// buildPIDTable assigns roles and fills the PID mapping table.
func (r *Report) buildPIDTable(reg *psi.Registry, counts map[uint16]*pidCount) {
	role := map[uint16]*PIDInfo{}
	get := func(pid uint16) *PIDInfo {
		pi := role[pid]
		if pi == nil {
			pi = &PIDInfo{PID: pid, Role: "unregistered"}
			role[pid] = pi
		}
		return pi
	}
	get(0).Role = "PAT"
	get(0x1FFF).Role = "null"
	if reg.PAT != nil {
		for _, pid := range reg.PAT.NITPIDs {
			get(pid).Role = "NIT"
		}
	}
	for _, p := range reg.Programs() {
		pi := get(p.PMTPID)
		pi.Role = "PMT"
		pi.Program = p.ProgramNumber
		if p.PMT == nil {
			continue
		}
		if pc := get(p.PMT.PCRPID); pc.Role == "unregistered" || pc.Role == "ES" {
			pc.Role = "PCR"
			pc.Program = p.ProgramNumber
		}
		for _, s := range p.PMT.Streams {
			si := get(s.PID)
			if si.PID == p.PMT.PCRPID {
				si.Role = "PCR+ES"
			} else {
				si.Role = "ES"
			}
			si.Program = p.ProgramNumber
			si.StreamType = fmt.Sprintf("0x%02X %s", s.StreamType, psi.StreamTypeName(s.StreamType))
		}
	}
	pids := make([]int, 0, len(counts))
	for pid := range counts {
		pids = append(pids, int(pid))
	}
	sort.Ints(pids)
	for _, p := range pids {
		pid := uint16(p)
		pi := get(pid)
		pi.Packets = counts[pid].packets
		pi.Bytes = counts[pid].bytes
		switch pi.Role {
		case "null":
			r.NullPackets += pi.Packets
		case "ES", "PCR+ES":
			r.ESPackets += pi.Packets
		case "PAT", "PMT", "NIT":
			r.PSIPackets += pi.Packets
		}
		r.PIDs = append(r.PIDs, *pi)
	}
}

// WriteJSON writes the report as indented JSON.
func (r *Report) WriteJSON(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
