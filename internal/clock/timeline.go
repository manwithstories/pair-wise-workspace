// Package clock defines the unified media time base shared by the demuxer,
// segment cutter, ladder aligner and manifest writer. All modules convert
// per-track timestamps into canonical ticks before comparing or cutting so
// that audio/video and multiple bitrate ladders never drift apart.
package clock

import (
	"fmt"

	"github.com/pion/rtp"
)

// Scale is the canonical unified timeline resolution in ticks per second.
// 90000 is the RTP video clock rate, so canonical ticks map losslessly onto
// RTP timestamps and exactly represent common MP4 timescales (90000, 48000,
// 44100, 15360, 1000 ...).
const Scale = 90000

// Ticks converts a timestamp expressed in an arbitrary media timescale into
// canonical timeline ticks, rounding to the nearest tick.
func Ticks(value int64, scale uint32) int64 {
	if scale == 0 {
		return 0
	}
	if scale == Scale {
		return value
	}
	num := value * int64(Scale)
	den := int64(scale)
	// Round to nearest, ties away from zero, to avoid systematic drift.
	if value >= 0 {
		return (num + den/2) / den
	}
	return (num - den/2) / den
}

// Duration converts a duration expressed in a media timescale to canonical
// ticks. It is identical to Ticks but documents intent at call sites.
func Duration(value int64, scale uint32) int64 { return Ticks(value, scale) }

// Millis renders canonical ticks as fractional milliseconds for reporting.
func Millis(ticks int64) float64 { return float64(ticks) * 1000.0 / float64(Scale) }

// TicksFromMillis converts whole milliseconds to canonical ticks.
func TicksFromMillis(ms int64) int64 { return ms * int64(Scale) / 1000 }

// TicksFromSeconds converts fractional seconds to canonical ticks.
func TicksFromSeconds(sec float64) int64 { return int64(sec*float64(Scale) + 0.5) }

// Seconds renders canonical ticks as fractional seconds.
func Seconds(ticks int64) float64 { return float64(ticks) / float64(Scale) }

// RTP maps canonical ticks onto an RTP timestamp for the given clock rate.
// The mapping is modulo 2^32, matching RTP timestamp wraparound semantics.
func RTP(ticks int64, clockRate uint32) uint32 {
	if clockRate == 0 {
		return 0
	}
	v := ticks * int64(clockRate) / int64(Scale)
	return uint32(v) //nolint:staticcheck // intentional mod 2^32 wrap
}

// CheckRTPSequence is an auxiliary validation helper: it encodes each
// canonical boundary as an RTP packet (using pion/rtp), round-trips the
// wire format, and confirms the decoded timestamps are strictly increasing
// for strictly increasing boundaries. It guards the segmenter against
// timestamp quantization that would collapse two distinct boundaries onto
// one RTP timestamp at the given clock rate and payload type.
func CheckRTPSequence(boundaries []int64, clockRate uint32, payloadType uint8) error {
	if clockRate == 0 {
		return fmt.Errorf("rtp clock rate must be positive")
	}
	var prev uint32
	for i, b := range boundaries {
		ts := RTP(b, clockRate)
		pkt := rtp.Packet{Header: rtp.Header{
			Version:        2,
			PayloadType:    payloadType,
			SequenceNumber: uint16(i), //nolint:gosec // test vector only
			Timestamp:      ts,
			SSRC:           0x6d706163,
		}}
		raw, err := pkt.Marshal()
		if err != nil {
			return fmt.Errorf("marshal rtp packet %d: %w", i, err)
		}
		var back rtp.Packet
		if err := back.Unmarshal(raw); err != nil {
			return fmt.Errorf("unmarshal rtp packet %d: %w", i, err)
		}
		if back.Header.Timestamp != ts {
			return fmt.Errorf("rtp timestamp round-trip mismatch at boundary %d", i)
		}
		if i > 0 && boundaries[i] > boundaries[i-1] && ts == prev {
			return fmt.Errorf("rtp timestamp collision at boundary %d (rate %d)", i, clockRate)
		}
		prev = ts
	}
	return nil
}
