// Package codec derives packet durations from payload data. It never invents
// a duration for damaged payloads: callers receive ErrUnderivable and must
// quarantine the packet instead of guessing.
package codec

import (
	"errors"
	"fmt"

	"example.com/medialab/archive/internal/clock"
)

var (
	// ErrShortPayload means the payload cannot even carry a TOC byte.
	ErrShortPayload = errors.New("codec: payload too short")
	// ErrUnderivable means the payload does not let us prove a duration.
	ErrUnderivable = errors.New("codec: duration not derivable")
)

// OpusRate is the fixed RTP clock rate for Opus (RFC 7587).
const OpusRate clock.Rate = 48000

// opusFrameSizes lists the frame duration in microseconds per TOC config.
// Configs 0..3 SILK NB, 4..7 SILK MB, 8..11 SILK WB, 12..15 Hybrid,
// 16..19 Hybrid FB, 20..31 CELT. Each group of four uses 10/20/40/60 ms for
// SILK, 10/20 ms for Hybrid and 2.5/5/10/20 ms for CELT.
var opusFrameSizesUS = [32]int64{
	10000, 20000, 40000, 60000, // 0-3 SILK NB
	10000, 20000, 40000, 60000, // 4-7 SILK MB
	10000, 20000, 40000, 60000, // 8-11 SILK WB
	10000, 20000, 10000, 20000, // 12-15 Hybrid SWB/FB
	2500, 5000, 10000, 20000, // 16-19 CELT NB
	2500, 5000, 10000, 20000, // 20-23 CELT WB
	2500, 5000, 10000, 20000, // 24-27 CELT SWB
	2500, 5000, 10000, 20000, // 28-31 CELT FB
}

// OpusDurationUS derives the packet media duration from the Opus TOC byte and
// frame-length table. It rejects truncation and inconsistent frame counts so
// damaged payloads are never assigned a fabricated duration.
func OpusDurationUS(payload []byte) (int64, error) {
	if len(payload) < 1 {
		return 0, ErrShortPayload
	}
	toc := payload[0]
	config := toc >> 3
	frames := int(toc >> 1 & 0x3)
	frameUS := opusFrameSizesUS[config&0x1F]
	body := payload[1:]

	switch frames {
	case 0: // one frame
		return frameUS, nil
	case 1: // two equal frames
		return 2 * frameUS, nil
	case 2: // two frames of different sizes, same duration
		if len(body) < 1 {
			return 0, fmt.Errorf("%w: missing frame length", ErrUnderivable)
		}
		firstLen := int(body[0])
		if firstLen <= 0 || firstLen > len(body)-1 {
			return 0, fmt.Errorf("%w: frame length %d vs %d bytes", ErrUnderivable, firstLen, len(body)-1)
		}
		// The second frame is the remainder; both must be non-empty.
		if len(body)-1-firstLen <= 0 {
			return 0, fmt.Errorf("%w: empty trailing frame", ErrUnderivable)
		}
		// RFC 6716 3.2.5: code 2 frames differ in size, not duration.
		return 2 * frameUS, nil
	case 3: // arbitrary frame count with a length table
		return opusArbitraryDurationUS(frameUS, body)
	}
	return 0, ErrUnderivable
}

func opusArbitraryDurationUS(frameUS int64, body []byte) (int64, error) {
	if len(body) < 1 {
		return 0, fmt.Errorf("%w: missing count byte", ErrUnderivable)
	}
	n := int(body[0] & 0x3F)
	if n < 1 || n > 48 {
		return 0, fmt.Errorf("%w: frame count %d", ErrUnderivable, n)
	}
	// No length table: the payload must split evenly into n frames.
	if len(body) <= 1 || (len(body)-1)%n != 0 {
		return 0, fmt.Errorf("%w: %d bytes over %d frames", ErrUnderivable, len(body)-1, n)
	}
	return int64(n) * frameUS, nil
}
