package rtp

import (
	"encoding/binary"
	"math"

	"mediamix/internal/clock"
)

// SenderReportPT is the RTCP payload type for Sender Reports (RFC 3550).
const SenderReportPT uint8 = 200

// srLen is the wire length of an SR with zero report blocks (28 bytes,
// length field = 6 32-bit words minus one).
const (
	srLen        = 28
	srWordsMinus = 6
)

// SenderReport is a parsed RTCP SR. NTP time is carried on the shared
// 90 kHz reference clock for deterministic local fixtures.
type SenderReport struct {
	SSRC         uint32
	NTPTick      clock.Tick // reference-clock instant of the report
	RTPTimestamp uint32
	PacketCount  uint32
	OctetCount   uint32
}

// MarshalSR serialises a Sender Report. The 90 kHz tick is encoded into the
// 64-bit NTP field (seconds + 32-bit fractional part) without loss at the
// rates this project uses.
func MarshalSR(sr SenderReport) []byte {
	buf := make([]byte, srLen)
	buf[0] = 0x80 // V=2, P=0, RC=0
	buf[1] = SenderReportPT
	binary.BigEndian.PutUint16(buf[2:4], srWordsMinus)
	binary.BigEndian.PutUint32(buf[4:8], sr.SSRC)
	secs := int64(sr.NTPTick) / int64(clock.TickRate)
	rem := int64(sr.NTPTick) - secs*int64(clock.TickRate)
	frac := uint32(math.Round(float64(rem) * (1 << 32) / float64(clock.TickRate)))
	binary.BigEndian.PutUint32(buf[8:12], uint32(secs))
	binary.BigEndian.PutUint32(buf[12:16], frac)
	binary.BigEndian.PutUint32(buf[16:20], sr.RTPTimestamp)
	binary.BigEndian.PutUint32(buf[20:24], sr.PacketCount)
	binary.BigEndian.PutUint32(buf[24:28], sr.OctetCount)
	return buf
}

// ParseSR validates and parses one RTCP SR datagram.
func ParseSR(data []byte) (*SenderReport, error) {
	if len(data) < srLen {
		return nil, clock.NewError(clock.StageRTCP, clock.ErrInvalidPacket, 0, false,
			"RTCP packet shorter than 28 byte sender report")
	}
	if data[0]>>6 != Version {
		return nil, clock.NewError(clock.StageRTCP, clock.ErrInvalidPacket, 0, false, "bad RTCP version")
	}
	if data[1] != SenderReportPT {
		return nil, clock.NewError(clock.StageRTCP, clock.ErrInvalidPacket, 0, false, "not a sender report")
	}
	rc := data[0] & 0x0F
	words := int(binary.BigEndian.Uint16(data[2:4])) + 1
	if len(data) < words*4 {
		return nil, clock.NewError(clock.StageRTCP, clock.ErrInvalidPacket, 0, false, "truncated RTCP length field")
	}
	sr := &SenderReport{
		SSRC:         binary.BigEndian.Uint32(data[4:8]),
		RTPTimestamp: binary.BigEndian.Uint32(data[16:20]),
		PacketCount:  binary.BigEndian.Uint32(data[20:24]),
		OctetCount:   binary.BigEndian.Uint32(data[24:28]),
	}
	secs := int64(binary.BigEndian.Uint32(data[8:12]))
	frac := binary.BigEndian.Uint32(data[12:16])
	rem := int64(math.Round(float64(frac) * float64(clock.TickRate) / (1 << 32)))
	sr.NTPTick = clock.Tick(secs*int64(clock.TickRate) + rem)
	_ = rc
	return sr, nil
}

// Calibrator owns the per-track timeline and counts calibration conditions
// so the metrics layer can report missing and rejected Sender Reports.
type Calibrator struct {
	tl           *clock.Timeline
	Reports      int
	Rejected     int
	NoReportUses int
}

// NewCalibrator builds a calibrator for the track's RTP clock rate.
func NewCalibrator(rtpClockRate uint32) *Calibrator {
	return &Calibrator{tl: clock.NewTimeline(rtpClockRate)}
}

// Apply feeds one Sender Report into the timeline. Rejected reports retain
// the last valid mapping and return a recoverable error.
func (c *Calibrator) Apply(sr *SenderReport) error {
	if err := c.tl.Calibrate(sr.RTPTimestamp, sr.NTPTick); err != nil {
		c.Rejected++
		return err
	}
	c.Reports++
	return nil
}

// ToTick maps an RTP timestamp to the shared clock. Before the first
// accepted report the provisional mapping is used and the no-report
// condition is counted (and returned as a recoverable error).
func (c *Calibrator) ToTick(rtp uint32) (clock.Tick, error) {
	t, err := c.tl.ToTick(rtp)
	if err != nil && clock.IsCode(err, clock.ErrNoReport) {
		c.NoReportUses++
	}
	return t, err
}

// Valid reports whether an accepted Sender Report exists.
func (c *Calibrator) Valid() bool { return c.tl.Valid() }
