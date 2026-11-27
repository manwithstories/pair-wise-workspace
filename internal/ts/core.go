package ts

import (
	"tsqc/internal/clock"
)

// Demux drives PSI assembly, continuity checking and PES reassembly. It is a
// pure state machine: the pipeline feeds packets and receives access units.
type Demux struct {
	tables   *TableManager
	sections map[uint16]*SectionAssembler
	cont     *Continuity
	timeline map[uint16]*clock.Timeline
	pes      map[uint16]*pesAccum
	streams  map[uint16]Kind
	pmtPID   uint16
	packetNo int
	stats    DemuxStats

	pcrPID       uint16
	lastPCR      int64
	pcrSeen      bool
	lastVideoPTS int64
	lastAudioPTS int64
	videoSeen    bool
	audioSeen    bool
	SyncSkew     int64
	syncFlagged  bool
	orderCounter int
}

// NewDemux builds an empty demultiplexer.
func NewDemux() *Demux {
	return &Demux{
		tables:       NewTableManager(),
		sections:     make(map[uint16]*SectionAssembler),
		cont:         NewContinuity(),
		timeline:     make(map[uint16]*clock.Timeline),
		pes:          make(map[uint16]*pesAccum),
		streams:      make(map[uint16]Kind),
		pcrPID:       PIDNull,
		lastVideoPTS: -1,
		lastAudioPTS: -1,
	}
}

// Tables exposes the active PSI tables.
func (d *Demux) Tables() *TableManager { return d.tables }

// Stats snapshots counters.
func (d *Demux) Stats() DemuxStats { return d.stats }

// PushPacket consumes one 188-byte packet and returns any access units that
// completed because a new PES started or a table switched the stream map.

// DemuxStats captures parsing outcomes for reports.
type DemuxStats struct {
	Packets       int
	PATs          int
	PMTs          int
	CCJumps       int
	PSIErrors     int
	VideoAUs      int
	AudioAUs      int
	IDRs          int
	Unrecoverable int
}
