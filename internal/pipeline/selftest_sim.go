package pipeline

import (
	"strconv"

	"avpipe/internal/clock"
	"avpipe/internal/rtp"
)

// simulateBurst sends total packets and removes a contiguous burst, then
// reports how the recovery window classified the gap.
func burstSim(opts Options, total, burst int) (rtp.Recovery, error) {
	sent := make([]rtp.Packet, 0, total-burst)
	for i := 0; i < total; i++ {
		if i >= total/2 && i < total/2+burst {
			continue
		}
		sent = append(sent, rtp.Packet{Seq: uint16(i), Timestamp: uint32(i * 960),
			SSRC: 0x1, Payload: []byte{byte(i)}})
	}
	return rtp.SimulateReceive(sent, rtp.Config{
		Seed: opts.Seed, Window: opts.RecoveryWindow, LossRate: 0})
}

func spanOf(ms float64) clock.Span {
	return clock.Span{Start: 0, End: clock.FromMillis(ms)}
}

func itoa(n int) string { return strconv.Itoa(n) }

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 3, 64) }
