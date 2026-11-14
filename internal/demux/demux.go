package demux

import (
	"fmt"

	"tsprobe/internal/psi"
	"tsprobe/internal/tspacket"
)

type Keyframe struct {
	PID          uint16
	Program      uint16
	PacketOffset int64
	ByteOffset   int64
	PTS          int64
	HasPTS       bool
}

type Stats struct {
	Packets       int64
	PSISections   int64
	CRCFailures   int64
	SectionErrors int64
	TableErrors   int64
	Keyframes     int64
}

// Demuxer feeds packet-layer data into the authoritative PSI store and tracks
// PES starts and video keyframes.
type Demuxer struct {
	Store        *psi.Store
	assembler    *psi.Assembler
	packetOffset int64
	byteOffset   int64
	lastPTS      int64
	hasLastPTS   bool
	stats        Stats
}

func New(store *psi.Store) *Demuxer {
	return &Demuxer{Store: store, assembler: psi.NewAssembler()}
}

func (d *Demuxer) Stats() Stats { return d.stats }

func (d *Demuxer) CRCFailures() int64 { return d.stats.CRCFailures }

func (d *Demuxer) LastVideoPTS() (int64, bool) { return d.lastPTS, d.hasLastPTS }

// Process handles one packet. A non-nil keyframe is returned exactly when the
// packet starts a keyframe PES.
func (d *Demuxer) Process(p *tspacket.Packet) (*Keyframe, error) {
	offset := d.packetOffset
	d.packetOffset++
	d.byteOffset += tspacket.PacketSize
	d.stats.Packets++
	if p.TEI || p.Duplicate || p.PID == tspacket.NullPID {
		return nil, nil
	}

	if d.Store.IsPSIPID(p.PID) {
		if !p.HasPayload {
			return nil, nil
		}
		for _, sec := range d.assembler.Feed(p.PID, p.PUSI, p.Payload) {
			d.stats.PSISections++
			if sec.Err != "" {
				d.stats.SectionErrors++
				continue
			}
			if !sec.CRCValid {
				d.stats.CRCFailures++
				continue
			}
			if err := d.Store.Consume(sec); err != nil {
				d.stats.TableErrors++
			}
		}
		d.Store.RecordPacket(p.PID)
		return nil, nil
	}
	if !p.HasPayload {
		return nil, nil
	}

	program, streamType, mapped := d.Store.LookupPID(p.PID)
	d.Store.RecordPacket(p.PID)
	if !mapped || !p.PUSI || !psi.IsVideoStreamType(streamType) {
		return nil, nil
	}
	pes, err := parsePES(p.Payload)
	if err != nil {
		return nil, err
	}
	if pes.hasPTS {
		d.lastPTS = pes.pts
		d.hasLastPTS = true
	}
	if !isKeyframe(streamType, pes.data, p.RandomAccess) {
		return nil, nil
	}
	d.stats.Keyframes++
	return &Keyframe{
		PID:          p.PID,
		Program:      program,
		PacketOffset: offset,
		ByteOffset:   d.byteOffset - tspacket.PacketSize,
		PTS:          pes.pts,
		HasPTS:       pes.hasPTS,
	}, nil
}

type pesInfo struct {
	pts    int64
	hasPTS bool
	data   []byte
}

func parsePES(payload []byte) (pesInfo, error) {
	if len(payload) < 9 || payload[0] != 0 || payload[1] != 0 || payload[2] != 1 {
		return pesInfo{data: payload}, nil
	}
	headerLen := int(payload[8])
	if 9+headerLen > len(payload) {
		return pesInfo{}, fmt.Errorf("demux: truncated PES header")
	}
	out := pesInfo{data: payload[9+headerLen:]}
	if payload[7]&0x80 != 0 && headerLen >= 5 && payload[9]&0xf0 == 0x20 {
		b := payload[9:14]
		out.pts = int64(b[0]&0x0e)<<29 | int64(b[1])<<22 | int64(b[2]&0xfe)<<14 |
			int64(b[3])<<7 | int64(b[4]>>1)
		out.hasPTS = true
	}
	return out, nil
}

func isKeyframe(streamType byte, data []byte, randomAccess bool) bool {
	if randomAccess {
		return true
	}
	for i := 0; i+4 <= len(data); i++ {
		if data[i] != 0 || data[i+1] != 0 || data[i+2] != 1 {
			continue
		}
		switch streamType {
		case 0x1b:
			if data[i+3]&0x1f == 5 {
				return true
			}
		case 0x24:
			nalType := (data[i+3] >> 1) & 0x3f
			if nalType >= 16 && nalType <= 21 {
				return true
			}
		case 0x01, 0x02:
			if i+7 <= len(data) && data[i+3] == 0 {
				// picture_coding_type 001 is an I picture
				if data[i+5]&0x07 == 1 {
					return true
				}
			}
		}
	}
	return false
}
