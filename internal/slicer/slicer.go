package slicer

import (
	"time"

	"tsprobe/internal/psi"
	"tsprobe/internal/tspkt"
)

type ProgramRevision struct {
	ProgramNumber uint16 `json:"program_number"`
	PMTPID        uint16 `json:"pmt_pid"`
	Revision      int    `json:"revision"`
	Version       int    `json:"version"`
}

type ProgramPIDs struct {
	ProgramNumber  uint16   `json:"program_number"`
	PMTPID         uint16   `json:"pmt_pid"`
	PCRPID         uint16   `json:"pcr_pid"`
	ElementaryPIDs []uint16 `json:"elementary_pids"`
}

type Switch struct {
	EffectivePacket int64    `json:"effective_packet"`
	Table           string   `json:"table"`
	ProgramNumber   uint16   `json:"program_number"`
	PMTPID          uint16   `json:"pmt_pid"`
	Version         int      `json:"version"`
	Revision        int      `json:"revision"`
	PMTPIDs         []uint16 `json:"pmt_pids"`
	PIDs            []uint16 `json:"pids"`
}

type Segment struct {
	Index             int               `json:"index"`
	StartPacket       int64             `json:"start_packet"`
	EndPacket         int64             `json:"end_packet"`
	StartPCR          int64             `json:"start_pcr"`
	EndPCR            int64             `json:"end_pcr"`
	DurationMS        int64             `json:"duration_ms"`
	PSIRevision       int               `json:"psi_revision"`
	PATRevision       int               `json:"pat_revision"`
	PATVersion        int               `json:"pat_version"`
	PMTRevisions      []ProgramRevision `json:"pmt_revisions"`
	PIDs              []ProgramPIDs     `json:"pids"`
	StartRandomAccess bool              `json:"start_random_access"`
	EndRandomAccess   bool              `json:"end_random_access"`
	PSISwitches       []Switch          `json:"psi_switches"`
}

type Builder struct {
	registry            *psi.Registry
	target              int64
	active              bool
	startPkt            int64
	startPCR            int64
	lastPCR             int64
	lastPkt             int64
	havePCR             bool
	pendingRandomAccess bool
	pendingPacket       int64
	segments            []Segment
}

func NewBuilder(target time.Duration, registry *psi.Registry) *Builder {
	return &Builder{registry: registry, target: target.Nanoseconds() * 27 / 1000}
}

func (b *Builder) Observe(pkt tspkt.Packet) {
	if pkt.Index > b.lastPkt {
		b.lastPkt = pkt.Index
	}
	if pkt.HasPCR {
		if !b.active {
			if b.pendingRandomAccess {
				b.active = true
				b.startPkt = b.pendingPacket
				b.startPCR = pkt.PCR
				b.pendingRandomAccess = false
			} else if pkt.RandomAccess {
				b.active = true
				b.startPkt = pkt.Index
				b.startPCR = pkt.PCR
			}
			b.lastPCR = pkt.PCR
			b.havePCR = true
			return
		}
		if pkt.RandomAccess && b.target > 0 && pkt.PCR-b.startPCR >= b.target {
			b.appendSegment(pkt.Index-1, pkt.PCR, true)
			b.startPkt = pkt.Index
			b.startPCR = pkt.PCR
		}
		b.lastPCR = pkt.PCR
		b.havePCR = true
		return
	}
	if !b.active {
		if pkt.RandomAccess {
			if b.havePCR {
				b.active = true
				b.startPkt = pkt.Index
				b.startPCR = b.lastPCR
			} else {
				b.pendingRandomAccess = true
				b.pendingPacket = pkt.Index
			}
		}
		return
	}
	if pkt.RandomAccess && b.target > 0 && b.lastPCR-b.startPCR >= b.target {
		b.appendSegment(pkt.Index-1, b.lastPCR, true)
		b.startPkt = pkt.Index
		b.startPCR = b.lastPCR
	}
}

func (b *Builder) appendSegment(endPacket, endPCR int64, endRandomAccess bool) {
	if endPacket < b.startPkt {
		endPacket = b.startPkt
	}
	duration := (endPCR - b.startPCR) / 27000
	if duration < 0 {
		duration = 0
	}
	b.segments = append(b.segments, Segment{
		StartPacket:       b.startPkt,
		EndPacket:         endPacket,
		StartPCR:          b.startPCR,
		EndPCR:            endPCR,
		DurationMS:        duration,
		StartRandomAccess: true,
		EndRandomAccess:   endRandomAccess,
	})
}

func (b *Builder) Finish(lastPacket int64) []Segment {
	if lastPacket > b.lastPkt {
		b.lastPkt = lastPacket
	}
	if b.active {
		b.appendSegment(b.lastPkt, b.lastPCR, false)
		b.active = false
	}
	for i := range b.segments {
		segment := &b.segments[i]
		segment.Index = i
		segment.PMTRevisions = []ProgramRevision{}
		segment.PIDs = []ProgramPIDs{}
		segment.PSISwitches = []Switch{}
		ctx := b.registry.ContextAt(segment.StartPacket)
		segment.PSIRevision = ctx.Epoch
		segment.PATRevision = ctx.PATRevision
		segment.PATVersion = int(ctx.PATVersion)
		for _, program := range ctx.Programs {
			segment.PMTRevisions = append(segment.PMTRevisions, ProgramRevision{
				ProgramNumber: program.ProgramNumber,
				PMTPID:        program.PMTPID,
				Revision:      program.PMTRevision,
				Version:       int(program.PMTVersion),
			})
			entry := ProgramPIDs{
				ProgramNumber:  program.ProgramNumber,
				PMTPID:         program.PMTPID,
				PCRPID:         program.PCRPID,
				ElementaryPIDs: []uint16{},
			}
			for _, stream := range program.Streams {
				entry.ElementaryPIDs = append(entry.ElementaryPIDs, stream.PID)
			}
			segment.PIDs = append(segment.PIDs, entry)
		}
		for _, event := range b.registry.EventsBetween(segment.StartPacket, segment.EndPacket) {
			segment.PSISwitches = append(segment.PSISwitches, Switch{
				EffectivePacket: event.EffectivePacket,
				Table:           event.Table,
				ProgramNumber:   event.ProgramNumber,
				PMTPID:          event.PMTPID,
				Version:         int(event.Version),
				Revision:        event.Revision,
				PMTPIDs:         event.PMTPIDs,
				PIDs:            event.PIDs,
			})
		}
	}
	return b.segments
}
