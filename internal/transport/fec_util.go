package transport

import "avpipe/internal/media"

// inferPTS estimates the timeline position of a missing slot from the
// block's surviving timestamps. Video uses the fixed 90 kHz frame cadence;
// audio interpolates from neighbouring slots.
func inferPTS(b *fecBlock, slot int) uint64 {
	if b.blockKind() == media.KindVideo {
		for s := 0; s < BlockSize; s++ {
			if b.slots[s] != nil {
				return b.pts[s] + uint64(slot-s)*media.ClockRate/media.VideoFPS
			}
		}
	}
	prev, next := -1, -1
	for s := 0; s < BlockSize; s++ {
		if b.slots[s] != nil {
			if s < slot {
				prev = s
			}
			if s > slot && next == -1 {
				next = s
			}
		}
	}
	switch {
	case prev >= 0 && next >= 0:
		step := (b.pts[next] - b.pts[prev]) / uint64(next-prev)
		return b.pts[prev] + step*uint64(slot-prev)
	case prev >= 0:
		return b.pts[prev] + audioStep(b)*uint64(slot-prev)
	case next >= 0:
		return b.pts[next] - audioStep(b)*uint64(next-slot)
	default:
		return uint64(slot) * media.ClockRate / media.VideoFPS
	}
}

func audioStep(b *fecBlock) uint64 {
	for s := 1; s < BlockSize; s++ {
		if b.slots[s] != nil && b.slots[s-1] != nil {
			return b.pts[s] - b.pts[s-1]
		}
	}
	return media.ClockRate / media.AudioPacketsPerSecond
}

func itoa(v int) string { return u32toa(uint32(v)) }

func u64toa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func putU32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

func putU64(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
}

func u32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func u64(b []byte) uint64 {
	var v uint64
	for i := 0; i < 8; i++ {
		v |= uint64(b[i]) << (8 * i)
	}
	return v
}
