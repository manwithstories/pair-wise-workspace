package ts

import (
	"fmt"

	"cinepipe/internal/clock"
	"cinepipe/internal/fault"
)

// PCR wraps the program clock reference: a 33-bit 90 kHz base plus a 9-bit
// 27 MHz extension.
type PCR struct {
	Base uint64 // 90 kHz ticks, 0..2^33-1
	Ext  uint16 // 27 MHz remainder, 0..299
}

// Valid reports whether the extension is within its legal 0..299 range.
func (p PCR) Valid() bool { return p.Ext < 300 }

// ParsePCR extracts a PCR from an adaptation field. The field must carry the
// PCR flag and have at least six bytes of raw payload. ok is false if either
// condition fails or the bytes yield an out-of-range extension.
func ParsePCR(af *AdaptationField) (PCR, bool) {
	if af == nil || !af.HasPCR || len(af.Raw) < 6 {
		return PCR{}, false
	}
	b := af.Raw
	base := uint64(b[0])<<25 | uint64(b[1])<<17 | uint64(b[2])<<9 |
		uint64(b[3])<<1 | uint64(b[4])>>7
	ext := uint16(b[4]&0x01)<<8 | uint16(b[5])
	p := PCR{Base: base & (clock.PCRModulus - 1), Ext: ext}
	if !p.Valid() {
		return p, false
	}
	return p, true
}

// BuildPCRBytes lays out six bytes for use inside an adaptation field. Reserved
// bits are set to the spec-mandated ones so the sample generator produces a
// compliant stream.
func BuildPCRBytes(p PCR) []byte {
	base := p.Base & (clock.PCRModulus - 1)
	return []byte{
		uint8(base >> 25),
		uint8(base >> 17),
		uint8(base >> 9),
		uint8(base >> 1),
		uint8(base<<7) | 0x7E | uint8(p.Ext>>8),
		uint8(p.Ext),
	}
}

// PCRTracker unwraps PCR values and flags genuine timing discontinuities. A
// wrap is expanded silently. A backwards step that is not a wrap (and not
// covered by an adaptation discontinuity_indicator) is a damaged range.
type PCRTracker struct {
	un    *clock.Unwrapper
	last  int64
	started bool
	origin int64
}

// NewPCRTracker returns a tracker bound to the 33-bit PCR modulus.
func NewPCRTracker() *PCRTracker {
	return &PCRTracker{un: clock.NewUnwrapper(clock.PCRModulus), origin: clock.NoTime}
}

// Observe unwraps a PCR. It returns the absolute 90 kHz base and any
// diagnostics. The wrap flag is true when this sample crossed the modulus.
func (t *PCRTracker) Observe(p *Packet, pcr PCR, discontinuity bool) (int64, bool, []fault.Error) {
	abs, wrapped := t.un.Unwrap(pcr.Base)
	if !t.started {
		t.started = true
		t.last = abs
		t.origin = abs
		return abs, false, nil
	}
	var errs []fault.Error
	delta := abs - t.last
	// A tolerance of one PCR interval (40 ms = 3600 ticks) accommodates legal
	// jitter; larger negative steps without a discontinuity indicator are
	// genuine reversions.
	if delta < -3600 && !discontinuity {
		errs = append(errs, p.Errorf(fault.KindContinuity,
			fmt.Sprintf("pcr rewind %d ticks", delta)))
	}
	t.last = abs
	return abs, wrapped, errs
}

// Origin returns the absolute tick of the first PCR, or clock.NoTime.
func (t *PCRTracker) Origin() int64 { return t.origin }

// Rel returns the first PCR relative to its origin.
func (t *PCRTracker) Rel(abs int64) int64 { return clock.Rel(t.origin, abs) }

// PES header layout helpers.
const pesStartCodeLen = 3

// PESTimestamp holds the optional PTS/DTS decoded from a PES header.
type PESTimestamp struct {
	StreamID  uint8
	PTS       uint64
	DTS       uint64
	HasPTS    bool
	HasDTS    bool
	DataStart int // offset in the payload where elementary data begins
	HeaderLen int
}

// ParsePESHeader decodes a PES header at the start of a PUSI payload. The
// payload must begin with the 0x000001 start code. ok is false when the bytes
// are not a PES header or the header is malformed; the returned error detail
// distinguishes truncation from a bad start code.
func ParsePESHeader(payload []byte) (PESTimestamp, bool, string) {
	var ts PESTimestamp
	if len(payload) < pesStartCodeLen {
		return ts, false, "payload too short for start code"
	}
	if payload[0] != 0x00 || payload[1] != 0x00 || payload[2] != 0x01 {
		return ts, false, fmt.Sprintf("bad PES start code %02x %02x %02x",
			payload[0], payload[1], payload[2])
	}
	if len(payload) < 9 {
		return ts, false, "PES header truncated"
	}
	ts.StreamID = payload[3]
	if ts.StreamID == 0xBE || ts.StreamID == 0xBF || ts.StreamID == 0xF0 ||
		ts.StreamID == 0xF1 || ts.StreamID == 0xFF || ts.StreamID == 0xF2 ||
		ts.StreamID == 0xF8 {
		// Padding/special streams carry no PTS.
		ts.DataStart = 6
		return ts, true, ""
	}
	if len(payload) < 7 {
		return ts, false, "PES length truncated"
	}
	flags := payload[7] >> 6
	headerDataLen := int(payload[8])
	ts.HeaderLen = headerDataLen
	pos := 9
	switch flags {
	case 0x2:
		if pos+5 > len(payload) {
			return ts, false, "PTS truncated"
		}
		ts.PTS = parsePTS(payload[pos:])
		ts.HasPTS = true
		pos += 5
	case 0x3:
		if pos+10 > len(payload) {
			return ts, false, "PTS/DTS truncated"
		}
		ts.PTS = parsePTS(payload[pos:])
		ts.DTS = parsePTS(payload[pos+5:])
		ts.HasPTS = true
		ts.HasDTS = true
		pos += 10
	}
	if headerDataLen+9 > len(payload) {
		return ts, false, "PES header length overruns payload"
	}
	ts.DataStart = 9 + headerDataLen
	return ts, true, ""
}

// parsePTS decodes a 5-byte timestamp using the MPEG marker-bit layout:
// b[0] holds bits 32..30, b[1] bits 29..22, b[2] bits 21..15, b[3] bits 14..7,
// b[4] bits 6..0, each byte prefixed by marker bits.
func parsePTS(b []byte) uint64 {
	return (uint64(b[0]>>1)&0x07)<<30 |
		uint64(b[1])<<22 |
		(uint64(b[2]>>1)&0x7F)<<15 |
		uint64(b[3])<<7 |
		(uint64(b[4]>>1)&0x7F)
}

// ExtendPTS expands a 33-bit PTS into an absolute tick using the nearest PCR
// epoch. PCR is a 33-bit base; several epochs may have elapsed between PCR and
// PTS carriage, so we choose the expansion closest to the reference absolute
// time.
func ExtendPTS(raw uint64, referenceAbs, referenceBasesSeen int64) int64 {
	raw %= clock.PTSModulus
	// Align the raw value to the modulus epoch nearest the reference.
	base := int64(raw)
	epochs := (referenceAbs - base) / int64(clock.PTSModulus)
	return base + epochs*int64(clock.PTSModulus)
}

// TimestampSet collects the first PTS and DTS seen for a PES stream, unwrapped
// onto the same absolute timeline as the PCR.
type TimestampSet struct {
	unPTS   *clock.Unwrapper
	unDTS   *clock.Unwrapper
	origin  int64
	firstPTS int64
	started bool
}

// NewTimestampSet returns a timestamp collector with the PTS modulus.
func NewTimestampSet() *TimestampSet {
	return &TimestampSet{
		unPTS:  clock.NewUnwrapper(clock.PTSModulus),
		unDTS:  clock.NewUnwrapper(clock.PTSModulus),
		origin: clock.NoTime,
	}
}

// Observe records a PES timestamp. It returns the absolute PTS (or NoTime when
// no PTS is present) and whether a wrap was crossed. The first observed PTS
// becomes the timeline origin.
func (t *TimestampSet) Observe(ts PESTimestamp) (int64, bool) {
	if !ts.HasPTS {
		return clock.NoTime, false
	}
	abs, wrapped := t.unPTS.Unwrap(ts.PTS)
	if !t.started {
		t.started = true
		t.firstPTS = abs
		t.origin = abs
	}
	if ts.HasDTS {
		t.unDTS.Unwrap(ts.DTS)
	}
	return abs, wrapped
}

// Origin returns the first PTS on the absolute timeline, or NoTime.
func (t *TimestampSet) Origin() int64 { return t.origin }

// Rel converts an absolute PTS to ticks relative to the first PTS.
func (t *TimestampSet) Rel(abs int64) int64 { return clock.Rel(t.origin, abs) }
