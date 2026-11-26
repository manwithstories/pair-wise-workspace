package rtp

import (
	"fmt"

	"medialab/internal/model"
)

// MaxAdaptiveQueue bounds queued timed frames independently of reorder cache.
const MaxAdaptiveQueue = 64

// TimedFrame is the small frame view used by adaptive jitter buffering.
type TimedFrame struct {
	Sequence   uint16
	TrackID    uint32
	PTSMS      int64
	DeadlineMS int64
	Keyframe   bool
}

// AdaptiveBuffer adjusts its target from bandwidth steps and removes expired
// non-key frames when the bounded queue is under pressure.
type AdaptiveBuffer struct {
	BandwidthKbps        int
	TargetMS             int
	Frames               []TimedFrame
	DroppedExpiredNonKey int
	OverflowDropped      int
}

// TargetForBandwidth maps lower bandwidth to a larger buffer. It is clamped to
// the hard 120 ms ceiling and a 20 ms floor.
func TargetForBandwidth(kbps int) int {
	if kbps <= 0 {
		return MaxBufferTargetMS
	}
	target := MaxBufferTargetMS - kbps/12
	if target < 20 {
		return 20
	}
	if target > MaxBufferTargetMS {
		return MaxBufferTargetMS
	}
	return target
}

// MaxBufferTargetMS is the hard adaptive-buffer ceiling.
const MaxBufferTargetMS = 120

// NewAdaptiveBuffer creates a buffer for one bandwidth estimate.
func NewAdaptiveBuffer(kbps int) *AdaptiveBuffer {
	return &AdaptiveBuffer{BandwidthKbps: kbps, TargetMS: TargetForBandwidth(kbps)}
}

// SetBandwidth applies the next bandwidth ladder rung and returns the target.
func (b *AdaptiveBuffer) SetBandwidth(kbps int) int {
	b.BandwidthKbps = kbps
	b.TargetMS = TargetForBandwidth(kbps)
	return b.TargetMS
}

// Enqueue stores a frame while enforcing the queue cap.
func (b *AdaptiveBuffer) Enqueue(f TimedFrame) error {
	if len(b.Frames) >= MaxAdaptiveQueue {
		b.OverflowDropped++
		return model.New(model.StageRecovery, "adaptive_queue_full", f.TrackID, int64(f.Sequence),
			fmt.Errorf("adaptive queue limit %d", MaxAdaptiveQueue))
	}
	b.Frames = append(b.Frames, f)
	return nil
}

// Sweep removes expired non-key frames. Expired keyframes are retained until a
// verified resume point decision is made by sync/report code.
func (b *AdaptiveBuffer) Sweep(nowMS int64) []TimedFrame {
	kept := b.Frames[:0]
	var dropped []TimedFrame
	for _, f := range b.Frames {
		if !f.Keyframe && f.DeadlineMS < nowMS {
			b.DroppedExpiredNonKey++
			dropped = append(dropped, f)
			continue
		}
		kept = append(kept, f)
	}
	b.Frames = kept
	return dropped
}

// QueuedDurationMS returns PTS span, a deterministic buffer-occupancy measure.
func (b *AdaptiveBuffer) QueuedDurationMS() int64 {
	if len(b.Frames) == 0 {
		return 0
	}
	min, max := b.Frames[0].PTSMS, b.Frames[0].PTSMS
	for _, f := range b.Frames[1:] {
		if f.PTSMS < min {
			min = f.PTSMS
		}
		if f.PTSMS > max {
			max = f.PTSMS
		}
	}
	return max - min
}
