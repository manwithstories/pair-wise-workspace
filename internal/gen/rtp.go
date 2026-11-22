package gen

import (
	"fmt"

	"github.com/pion/rtp"

	"mediascan/internal/clock"
)

// NetParams controls the deterministic network impairment injection.
type NetParams struct {
	Seed   int64
	Window uint16
	// Fixed positions guarantee the acceptance metrics are always
	// observable regardless of the random seed.
	DupFrame    int // frame whose RTP packet is duplicated
	ReorderPair int // frame swapped with its successor
	BurstFrom   int // first frame index of a burst-loss run
	BurstTo     int // last frame index of the burst-loss run
}

// DefaultNetParams returns impairments valid for any frame count >= 40.
func DefaultNetParams(seed int64) NetParams {
	return NetParams{
		Seed: seed, Window: 64,
		DupFrame:    5,
		ReorderPair: 12,
		BurstFrom:   24,
		BurstTo:     27,
	}
}

// RTPTrace turns the frame set into an ordered arrival list of RTP packets
// on the 90 kHz media clock, deterministically applying duplicate,
// reorder and burst-loss impairments. Sequence numbers are contiguous from
// the chosen start; RTP timestamps follow the frame tick grid and wrap by
// the 32-bit RTP rule when a high start timestamp is requested.
func RTPTrace(clk clock.Clock, frames int, startSeq uint16, startTS uint32, np NetParams) ([]*rtp.Packet, error) {
	if frames <= 0 {
		return nil, fmt.Errorf("gen: no frames for RTP trace")
	}
	if np.BurstTo >= frames || np.ReorderPair+1 >= frames || np.DupFrame >= frames {
		return nil, fmt.Errorf("gen: network impairment positions exceed frame count")
	}
	dropped := map[int]bool{}
	for i := np.BurstFrom; i <= np.BurstTo; i++ {
		dropped[i] = true
	}
	type slot struct {
		frame int
		seq   uint16
	}
	var slots []slot
	seq := startSeq
	for i := 0; i < frames; i++ {
		if dropped[i] {
			seq++ // gap in sequence space marks the loss
			continue
		}
		slots = append(slots, slot{frame: i, seq: seq})
		seq++
	}
	// swap arrival order of ReorderPair and its successor (both present)
	for i := range slots {
		if slots[i].frame == np.ReorderPair {
			for j := range slots {
				if slots[j].frame == np.ReorderPair+1 && j == i+1 {
					slots[i], slots[j] = slots[j], slots[i]
				}
			}
		}
	}
	out := make([]*rtp.Packet, 0, len(slots)+1)
	for _, sl := range slots {
		ts := clock.Wrap32(int64(startTS) + int64(sl.frame)*clk.TicksPerFrame())
		out = append(out, mkPacket(sl.seq, ts, sl.frame))
		if sl.frame == np.DupFrame {
			out = append(out, mkPacket(sl.seq, ts, sl.frame))
		}
	}
	return out, nil
}

func mkPacket(seq uint16, ts uint32, frame int) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           0x4d53434e,
		},
		Payload: []byte{byte(frame), byte(frame >> 8)},
	}
}
