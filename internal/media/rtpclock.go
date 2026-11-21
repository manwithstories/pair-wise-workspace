package media

import (
	"errors"
	"fmt"

	"github.com/pion/rtp"
)

// RTPClock expresses a track's timestamps on the RTP clock that delivery
// systems expect. It reuses pion/rtp for the packet header and sequence space
// so the arithmetic matches what a receiver will actually observe.
//
// Video uses the conventional 90 kHz rate; audio uses its sample rate. Values
// are 32-bit and wrap, exactly as on the wire, while the underlying sample
// timestamps stay exact rationals.
type RTPClock struct {
	rate uint32
	seq  rtp.Sequencer
	u    *Unwrapper
}

// NewRTPClock creates a clock at rate Hz. The sequence numbers start at zero
// so repeated runs produce identical output.
func NewRTPClock(rate uint32) (*RTPClock, error) {
	if rate == 0 {
		return nil, NewError(ClassUsage, "RTP clock rate must be positive")
	}
	return &RTPClock{rate: rate, seq: rtp.NewFixedSequencer(0), u: NewUnwrapper(32)}, nil
}

// Rate returns the clock rate in Hz.
func (c *RTPClock) Rate() uint32 { return c.rate }

// Timestamp converts an exact presentation time to a 32-bit RTP timestamp.
func (c *RTPClock) Timestamp(at Rat) uint32 {
	ticks := RatToTicks(at, c.rate)
	return uint32(uint64(ticks) & 0xffffffff)
}

// Extend expands a received 32-bit timestamp using per-track continuity.
// inputOffset is the byte offset of the timestamp, reported verbatim when the
// transition is ambiguous.
func (c *RTPClock) Extend(raw uint32, inputOffset int64) (int64, error) {
	return c.u.Unwrap(uint64(raw), uint64(inputOffset))
}

// NextSequence allocates the next sequence number for an outgoing stream.
func (c *RTPClock) NextSequence() uint16 { return c.seq.NextSequenceNumber() }

// RTPBoundary is the location of one RTP packet inside a buffer.
type RTPBoundary struct {
	Offset  int
	Length  int
	Header  rtp.Header
	Payload []byte
}

// ParseRTPDatagram decodes exactly one RTP packet. pion/rtp owns the header
// layout, extension parsing, padding and the header length, so the boundary
// arithmetic is the library's rather than a second implementation.
//
// A bare concatenation of RTP packets has no framing: nothing in the bitstream
// says where one payload ends and the next header begins. Boundaries are
// therefore only meaningful when each packet's extent is supplied by the
// caller (a file format, a length-prefixed log, a transport that preserves
// datagram sizes), which is what ParseRTPPackets accepts.
func ParseRTPDatagram(buf []byte) (RTPBoundary, error) {
	var p rtp.Packet
	if err := p.Unmarshal(buf); err != nil {
		return RTPBoundary{}, NewError(ClassInvalidLength, "invalid RTP packet: %v", err).
			WithOffset(0).WithTolerance(0).WithCause(err)
	}
	n := p.Header.MarshalSize() + len(p.Payload)
	if p.Header.Padding {
		n += int(p.PaddingSize)
	}
	if n <= 0 || n > len(buf) {
		return RTPBoundary{}, NewError(ClassInvalidLength,
			"RTP packet has no well-defined boundary").WithOffset(0).WithTolerance(0)
	}
	return RTPBoundary{Offset: 0, Length: n, Header: p.Header, Payload: p.Payload}, nil
}

// ParseRTPPackets decodes a sequence of datagrams whose extents the caller
// already knows. Each slice is one packet; a malformed packet is reported with
// the byte offset of its first byte in the original buffer.
func ParseRTPPackets(packets [][]byte) ([]RTPBoundary, error) {
	var out []RTPBoundary
	off := 0
	for _, buf := range packets {
		b, err := ParseRTPDatagram(buf)
		if err != nil {
			var e *Error
			if errors.As(err, &e) {
				e.WithOffset(int64(off))
			}
			return nil, err
		}
		b.Offset = off
		out = append(out, b)
		off += b.Length
	}
	return out, nil
}

// RTPInfo is the delivery-clock summary for one track.
type RTPInfo struct {
	Rate    uint32
	First   uint32
	Last    uint32
	Wraps   int64
	Samples int
}

// Summarize returns the RTP clock range for a track's index, expanding every
// timestamp through a fresh unwrapper so wrap counts are per track.
func (c *RTPClock) Summarize(idx SampleIndex) (RTPInfo, error) {
	info := RTPInfo{Rate: c.rate}
	if idx == nil || idx.Count() == 0 {
		return info, nil
	}
	u := NewUnwrapper(32)
	first, err := idx.At(0)
	if err != nil {
		return info, err
	}
	last, err := idx.At(idx.Count() - 1)
	if err != nil {
		return info, err
	}
	info.First = c.Timestamp(first.PTS)
	info.Last = c.Timestamp(last.PTS)
	info.Samples = idx.Count()
	for i := 0; i < idx.Count(); i++ {
		s, err := idx.At(i)
		if err != nil {
			return info, err
		}
		if _, err := u.Unwrap(uint64(c.Timestamp(s.PTS)), uint64(s.Offset)); err != nil {
			return info, WithTrackContext(err, "", 0)
		}
	}
	info.Wraps = u.WrapCount()
	return info, nil
}

func (i RTPInfo) String() string {
	return fmt.Sprintf("rate=%d first=%d last=%d wraps=%d", i.Rate, i.First, i.Last, i.Wraps)
}
