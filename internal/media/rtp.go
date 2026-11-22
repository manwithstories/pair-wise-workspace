package media

import (
	"github.com/pion/rtp"

	"mediacore/internal/clock"
)

// RTPUnwrapper extends 32-bit RTP timestamps into a monotonic 64-bit
// domain so wrap-around (timestamp overflow) does not corrupt the
// timeline.
type RTPUnwrapper struct {
	init    bool
	cycles  uint64
	lastExt uint64
}

// Unwrap converts one RTP timestamp into the extended domain.
func (u *RTPUnwrapper) Unwrap(ts uint32) uint64 {
	ext := u.cycles<<32 | uint64(ts)
	if u.init {
		if ext < u.lastExt && u.lastExt-ext > 1<<31 {
			// Forward wrap: the timestamp crossed 2^32.
			u.cycles++
			ext += 1 << 32
		}
	}
	u.lastExt = ext
	u.init = true
	return ext
}

// TicksFromRTP converts a packet sequence into rational ticks on the
// shared clock, unwrapping timestamp overflow. Packets must be in
// capture order.
func TicksFromRTP(packets []*rtp.Packet, timescale int64) ([]clock.Tick, error) {
	const op = "rtp-ticks"
	if timescale <= 0 {
		return nil, clock.At(clock.CodeTimescaleInvalid, op, "", -1,
			"timescale %d", timescale)
	}
	var u RTPUnwrapper
	ticks := make([]clock.Tick, 0, len(packets))
	for i, p := range packets {
		if p == nil {
			return nil, clock.At(clock.CodeInputQuality, op, "", i, "nil RTP packet")
		}
		ext := u.Unwrap(p.Timestamp)
		if ext > 1<<62 {
			return nil, clock.At(clock.CodeTickOverflow, op, "", i,
				"unwrapped timestamp %d too large", ext)
		}
		t, err := clock.New(int64(ext), timescale)
		if err != nil {
			return nil, clock.Wrap(clock.CodeTickOverflow, op, err)
		}
		ticks = append(ticks, t)
	}
	return ticks, nil
}
