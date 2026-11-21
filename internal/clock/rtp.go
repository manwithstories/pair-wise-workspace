package clock

import (
	"fmt"
	"time"

	"github.com/pion/rtp"
)

// Standard RTP clock rates used for the summary view.
const (
	RTPVideoClockRate uint32 = 90000
	RTPAudioClockRate uint32 = 48000
)

// RTPTimestamp converts a media timestamp at timescale into the 32-bit RTP
// timestamp domain at clockRate. The conversion is exact integer arithmetic;
// the result deliberately wraps modulo 2^32 as RTP requires.
func RTPTimestamp(ticks uint64, timescale, clockRate uint32) (uint32, error) {
	if timescale == 0 || clockRate == 0 {
		return 0, fmt.Errorf("clock: RTP conversion needs non-zero timescales (%d -> %d)",
			timescale, clockRate)
	}
	v, err := scale(ticks, uint64(clockRate), uint64(timescale))
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}

// CaptureExtension builds the RTP absolute-capture-time header extension for a
// media timestamp. Only the header extension is constructed; no network
// traffic is ever received or sent.
func CaptureExtension(ticks uint64, timescale uint32, epoch time.Time) (*rtp.AbsCaptureTimeExtension, error) {
	if timescale == 0 {
		return nil, fmt.Errorf("clock: zero timescale")
	}
	us, err := Micros(ticks, timescale)
	if err != nil {
		return nil, err
	}
	capture := epoch.Add(time.Duration(us) * time.Microsecond)
	return rtp.NewAbsCaptureTimeExtension(capture), nil
}

// Packet stamps a payload with an RTP header carrying the media timestamp and
// the absolute-capture-time extension, then round-trips it through the wire
// format so malformed headers fail here rather than downstream.
func Packet(ticks uint64, timescale uint32, clockRate uint32, seq uint16, ssrc uint32,
	payloadType uint8, epoch time.Time, payload []byte) (*rtp.Packet, error) {
	ts, err := RTPTimestamp(ticks, timescale, clockRate)
	if err != nil {
		return nil, err
	}
	ext, err := CaptureExtension(ticks, timescale, epoch)
	if err != nil {
		return nil, err
	}
	raw, err := ext.Marshal()
	if err != nil {
		return nil, fmt.Errorf("clock: marshal capture extension: %w", err)
	}
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    payloadType,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
		},
		Payload: payload,
	}
	// Leave Extension false so pion selects the RFC 8285 one-byte profile
	// from the payload length and accepts a non-zero extension id.
	if err := pkt.SetExtension(1, raw); err != nil {
		return nil, fmt.Errorf("clock: set capture extension: %w", err)
	}
	buf, err := pkt.Marshal()
	if err != nil {
		return nil, fmt.Errorf("clock: marshal packet: %w", err)
	}
	var decoded rtp.Packet
	if err := decoded.Unmarshal(buf); err != nil {
		return nil, fmt.Errorf("clock: verify packet: %w", err)
	}
	if decoded.Timestamp != ts {
		return nil, fmt.Errorf("clock: RTP timestamp changed in round trip")
	}
	return &decoded, nil
}

// CaptureTime extracts the absolute capture time from a packet extension.
func CaptureTime(pkt *rtp.Packet) (time.Time, bool) {
	if pkt == nil {
		return time.Time{}, false
	}
	raw := pkt.GetExtension(1)
	if len(raw) == 0 {
		return time.Time{}, false
	}
	var ext rtp.AbsCaptureTimeExtension
	if err := ext.Unmarshal(raw); err != nil {
		return time.Time{}, false
	}
	return ext.CaptureTime(), true
}
