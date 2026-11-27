package pipeline

import (
	"time"

	"mediacheck/internal/model"
)

// recoveredPacket turns a filled slot into a muxable audio packet.  PTS is
// in the source timebase so the muxer's rescaler sees the same unit as real
// packets.
func recoveredPacket(near model.Packet, slot model.Slot, data []byte) model.Packet {
	pts := near.Timebase.Ticks(time.Duration(slot.Start))
	dur := near.Timebase.Ticks(time.Duration(slot.Duration()))
	if dur <= 0 {
		dur = near.Duration
	}
	return model.Packet{
		Track:    model.TrackAudio,
		Codec:    near.Codec,
		Sequence: near.Sequence,
		PTS:      pts,
		DTS:      pts,
		Duration: dur,
		Keyframe: true,
		Data:     data,
		Timebase: near.Timebase,
		Slot:     slot,
	}
}

// splitGap divides a hole into whole previous-frame slots plus a remainder,
// so every filled slot has a plausible duration.
func splitGap(start, gap, per int64) []model.Slot {
	if per <= 0 || gap <= 0 {
		return nil
	}
	var out []model.Slot
	for gap > 0 {
		n := per
		if n > gap {
			n = gap
		}
		out = append(out, model.Slot{Start: start, End: start + n})
		start += n
		gap -= n
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
