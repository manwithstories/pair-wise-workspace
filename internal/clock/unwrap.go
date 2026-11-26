package clock

import (
	"archivectl/internal/core"
)

const wrapModulus int64 = 1 << 32
const halfWrap int64 = 1 << 31

// Anchor pairs a WAV cue/keyframe position with its raw 32-bit device tick.
type Anchor struct {
	CueID uint32 `json:"cue_id"`
	Tick  uint32 `json:"device_tick"`
	Frame int64  `json:"frame"`
}

// BlockRecord is one variable-length acquisition block. FrameCount is read
// from the sample/block record rather than inferred from a fixed block size.
type BlockRecord struct {
	Index      int    `json:"index"`
	Tick       uint32 `json:"device_tick"`
	FrameCount int    `json:"frame_count"`
}

// NormalizedAnchor is an anchor on the archive's sample-frame timebase.
type NormalizedAnchor struct {
	CueID         uint32 `json:"cue_id"`
	RawTick       uint32 `json:"raw_tick"`
	UnwrappedTick int64  `json:"unwrapped_tick"`
	WAVFrame      int64  `json:"wav_frame"`
	MappedFrame   int64  `json:"mapped_frame"`
	FrameError    int64  `json:"frame_error"`
}

// NormalizedBlock carries exact cumulative sample bounds and clock mapping.
type NormalizedBlock struct {
	Index         int    `json:"index"`
	RawTick       uint32 `json:"raw_tick"`
	UnwrappedTick int64  `json:"unwrapped_tick"`
	StartFrame    int64  `json:"start_frame"`
	EndFrame      int64  `json:"end_frame"`
	FrameCount    int    `json:"frame_count"`
	MappedFrame   int64  `json:"mapped_frame"`
}

// UnwrapTicks expands monotonically increasing 32-bit device timestamps.
// A forward wrap is detected when the signed delta is smaller than -2^31.
func UnwrapTicks(raw []uint32) ([]int64, error) {
	out := make([]int64, len(raw))
	if len(raw) == 0 {
		return out, nil
	}
	out[0] = int64(raw[0])
	var wraps int64
	for i := 1; i < len(raw); i++ {
		prev := int64(raw[i-1])
		cur := int64(raw[i])
		delta := cur - prev
		switch {
		case delta < -halfWrap:
			wraps++
			delta += wrapModulus
		case delta > halfWrap:
			// A backward jump across the boundary is not expected from the
			// acquisition clock; undo a wrap if records are replayed backwards.
			wraps--
			delta -= wrapModulus
		}
		out[i] = int64(raw[i]) + wraps*wrapModulus
		if out[i] < out[i-1] {
			return nil, core.Newf(core.KindClock, "clock.UnwrapTicks",
				"unwrapped clock is not monotonic at index %d: %d < %d", i, out[i], out[i-1])
		}
	}
	return out, nil
}

// CumulativeRanges converts variable block frame counts into exact ranges.
func CumulativeRanges(counts []int) ([]NormalizedBlock, error) {
	var total int64
	out := make([]NormalizedBlock, len(counts))
	for i, n := range counts {
		if n < 0 {
			return nil, core.Newf(core.KindClock, "clock.CumulativeRanges",
				"block %d has negative frame count %d", i, n)
		}
		out[i].Index = i
		out[i].StartFrame = total
		total += int64(n)
		out[i].EndFrame = total
		out[i].FrameCount = n
	}
	return out, nil
}

// UnwrapRecords is a small helper for callers that need raw record ticks.
func UnwrapRecords(records []BlockRecord) ([]int64, error) {
	raw := make([]uint32, len(records))
	for i := range records {
		raw[i] = records[i].Tick
	}
	return UnwrapTicks(raw)
}
