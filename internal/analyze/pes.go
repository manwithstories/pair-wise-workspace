package analyze

import (
	"fmt"

	"tscheck/internal/model"
	"tscheck/internal/ts"
)

// MaxPESBytes bounds a single PES buffer; larger declared lengths are
// rejected to cap memory per branch.
const MaxPESBytes = 1 << 20

// PES is one reassembled PES packet.
type PES struct {
	PID      uint16
	Offset   int64 // byte offset of its first TS packet
	PTSRaw   int64 // raw 33-bit 90 kHz PTS
	HasPTS   bool
	ES       []byte // elementary stream bytes after the PES header
	Complete bool
}

// PESAssembler rebuilds PES packets for one PID from TS packets.
type PESAssembler struct {
	pid       uint16
	buf       []byte
	open      bool
	declared  int // PES_packet_length, 0 = unbounded video
	esStart   int // offset of ES data inside buf, -1 if header invalid
	startOff  int64
	truncated bool
	ptsRaw    int64
	hasPTS    bool
}

// NewPESAssembler creates an assembler for pid.
func NewPESAssembler(pid uint16) *PESAssembler {
	return &PESAssembler{pid: pid, esStart: -1}
}

// Feed consumes one packet. It returns a completed PES (or a boundary
// error for the interrupted previous PES) plus any fault.
func (a *PESAssembler) Feed(pi ts.PacketInfo) (*PES, *model.Fault) {
	if pi.PUSI {
		var prevFault *model.Fault
		if a.open && !a.truncated && !a.completeLocked() {
			prevFault = &model.Fault{Kind: model.FaultIncompletePES,
				Offset: a.startOff, PID: int(a.pid),
				Detail: "PES interrupted by next unit start"}
		}
		a.reset(pi.Offset)
		if f := a.parseHeader(pi); f != nil {
			return nil, f
		}
		if a.completeLocked() {
			return a.emit(), prevFault
		}
		return nil, prevFault
	}
	if !a.open || a.truncated {
		return nil, nil
	}
	a.buf = append(a.buf, pi.Payload...)
	if len(a.buf) > MaxPESBytes {
		a.truncated = true
		return nil, &model.Fault{Kind: model.FaultOversizePES,
			Offset: a.startOff, PID: int(a.pid),
			Detail: fmt.Sprintf("PES exceeds %d byte buffer limit", MaxPESBytes)}
	}
	if a.completeLocked() {
		return a.emit(), nil
	}
	return nil, nil
}

// Flush ends the stream: an open, incomplete PES yields a boundary fault.
func (a *PESAssembler) Flush() *model.Fault {
	if a.open && !a.truncated && !a.completeLocked() {
		f := model.Fault{Kind: model.FaultIncompletePES, Offset: a.startOff,
			PID: int(a.pid), Detail: "stream ended mid-PES"}
		a.reset(0)
		return &f
	}
	a.reset(0)
	return nil
}

func (a *PESAssembler) reset(off int64) {
	a.buf = a.buf[:0]
	a.open = false
	a.truncated = false
	a.declared = 0
	a.esStart = -1
	a.startOff = off
	a.ptsRaw = 0
	a.hasPTS = false
}

// completeLocked reports whether the declared PES length is satisfied.
func (a *PESAssembler) completeLocked() bool {
	return a.open && a.declared > 0 && len(a.buf) >= 6+a.declared
}

// parseHeader validates and starts a new PES from a PUSI packet.
func (a *PESAssembler) parseHeader(pi ts.PacketInfo) *model.Fault {
	p := pi.Payload
	if len(p) < 9 || p[0] != 0x00 || p[1] != 0x00 || p[2] != 0x01 {
		return &model.Fault{Kind: model.FaultInvalidTS, Offset: pi.Offset,
			PID: int(a.pid), Detail: "bad PES start prefix"}
	}
	a.declared = int(p[4])<<8 | int(p[5])
	if a.declared > MaxPESBytes {
		a.truncated = true
		a.open = true
		return &model.Fault{Kind: model.FaultOversizePES, Offset: pi.Offset,
			PID: int(a.pid),
			Detail: fmt.Sprintf("declared PES length %d exceeds limit", a.declared)}
	}
	hdlen := int(p[8])
	a.esStart = 9 + hdlen
	a.open = true
	a.buf = append(a.buf, p...)
	if a.esStart > len(a.buf) {
		return nil // header continues in next packet
	}
	if p[7]&0x80 != 0 { // PTS present
		if len(p) < 14 || (p[9]>>4 != 0x02 && p[9]>>4 != 0x03) {
			return &model.Fault{Kind: model.FaultInvalidTS, Offset: pi.Offset,
				PID: int(a.pid), Detail: "bad PTS marker"}
		}
		a.ptsRaw = ptsFromHeader(p[9:14])
		a.hasPTS = true
	}
	return nil
}

// ptsFromHeader decodes the 5-byte 33-bit PTS field.
func ptsFromHeader(b []byte) int64 {
	return int64(b[0]>>1&0x07)<<30 |
		int64(b[1])<<22 |
		int64(b[2]>>1)<<15 |
		int64(b[3])<<7 |
		int64(b[4]>>1)
}

// emit returns the completed PES and resets the assembler.
func (a *PESAssembler) emit() *PES {
	end := len(a.buf)
	if a.declared > 0 && 6+a.declared < end {
		end = 6 + a.declared
	}
	es := []byte(nil)
	if a.esStart >= 0 && a.esStart <= end {
		es = append([]byte(nil), a.buf[a.esStart:end]...)
	}
	pes := &PES{PID: a.pid, Offset: a.startOff, PTSRaw: a.ptsRaw,
		HasPTS: a.hasPTS, ES: es, Complete: true}
	a.reset(0)
	return pes
}
