package transport

import (
	"mediaroute/internal/fault"
)

// HardMemoryLimit is the absolute ceiling for the jitter buffer. The
// bandwidth-adaptive target may shrink under this value but can never
// exceed it, guaranteeing the 256 MiB project budget.
const HardMemoryLimit = 64 << 20 // 64 MiB packet cache ceiling

// DefaultSoftLimit is the initial soft capacity before bandwidth is known.
const DefaultSoftLimit = 4 << 20 // 4 MiB

// JitterBuffer sits above the ReorderBuffer and applies back-pressure
// policy: the soft byte budget tracks estimated bandwidth while a hard
// memory limit guarantees bounded allocation. A sustained bandwidth drop
// pauses commit (keeping checkpoints intact); recovery resumes commit.
type JitterBuffer struct {
	reorder *ReorderBuffer
	bytes   int
	soft    int
	hard    int
	paused  bool
	// bandwidth history in bits per second.
	bwSamples      []int64
	lowStreak      int
	pauseStreak    int
	resumeStreak   int
	pauseThreshold float64 // fraction of nominal rate that counts as slow
}

// NewJitterBuffer wraps a reorder buffer with bandwidth policy.
func NewJitterBuffer(r *ReorderBuffer) *JitterBuffer {
	return &JitterBuffer{
		reorder:        r,
		soft:           DefaultSoftLimit,
		hard:           HardMemoryLimit,
		pauseThreshold: 0.45,
	}
}

// SetHardLimit overrides the hard memory ceiling. This exists so tests can
// exercise the ceiling without allocating the full production budget;
// production code keeps the 64 MiB default.
func (j *JitterBuffer) SetHardLimit(limit int) { j.hard = limit }

// ObserveBandwidth feeds one bandwidth estimate (bits per second) together
// with the nominal provisioned rate. A sustained estimate below the pause
// threshold flips the buffer into paused mode; recovery above 80% of
// nominal clears it.
func (j *JitterBuffer) ObserveBandwidth(estimateBps, nominalBps int64) {
	const window = 8
	j.bwSamples = append(j.bwSamples, estimateBps)
	if len(j.bwSamples) > window {
		j.bwSamples = j.bwSamples[1:]
	}
	if nominalBps <= 0 {
		return
	}
	avg := j.averageBW()
	ratio := float64(avg) / float64(nominalBps)
	// While paused the window is dominated by slow samples, so recovery is
	// judged on the latest observation; normal operation uses the average.
	latestRatio := float64(estimateBps) / float64(nominalBps)
	healthy := ratio > 0.8
	if j.paused {
		healthy = latestRatio > 0.8
	}
	switch {
	case ratio < j.pauseThreshold && !j.paused:
		j.lowStreak++
		j.resumeStreak = 0
	case healthy:
		j.resumeStreak++
		j.lowStreak = 0
	default:
		// Mid-range: decay both streaks rather than freezing them.
		if j.lowStreak > 0 {
			j.lowStreak--
		}
		if j.resumeStreak > 0 {
			j.resumeStreak--
		}
	}
	// Two consecutive slow windows pause; two healthy windows resume.
	if j.lowStreak >= 2 && !j.paused {
		j.paused = true
		j.pauseStreak++
	}
	if j.resumeStreak >= 2 && j.paused {
		j.paused = false
	}
	// Adapt the soft budget to the estimate: faster links tolerate more
	// in-flight bytes, slow links shrink it. Never cross the hard limit.
	target := int(float64(j.soft) * ratio)
	if target < 64<<10 {
		target = 64 << 10
	}
	if target > j.hard {
		target = j.hard
	}
	// Smooth to avoid flapping.
	j.soft = (j.soft*3 + target) / 4
	if j.soft > j.hard {
		j.soft = j.hard
	}
}

func (j *JitterBuffer) averageBW() int64 {
	if len(j.bwSamples) == 0 {
		return 0
	}
	var sum int64
	for _, v := range j.bwSamples {
		sum += v
	}
	return sum / int64(len(j.bwSamples))
}

// Enqueue attempts to store a packet, enforcing both the soft policy and
// the hard memory ceiling. While paused, packets are still accepted into
// the reorder buffer but commits are withheld by the caller.
func (j *JitterBuffer) Enqueue(seq uint16, payload []byte, nowNanos int64) (PushResult, *fault.Error) {
	add := len(payload)
	if add > j.hard || j.bytes+add > j.hard {
		return PushResult{Overflow: true}, fault.New(fault.CatLimitExceeded,
			"jitter buffer hard limit %d bytes exceeded (packet=%d, buffered=%d)",
			j.hard, add, j.bytes)
	}
	res := j.reorder.Push(seq, payload, nowNanos)
	if res.Accepted {
		j.bytes += add
	}
	return res, nil
}

// Dequeue returns contiguous in-order packets that are safe to commit.
// Returns nil while paused so callers preserve their checkpoint instead
// of advancing the output.
func (j *JitterBuffer) Dequeue() [][]byte {
	if j.paused {
		return nil
	}
	out := j.reorder.Drain()
	for _, p := range out {
		j.bytes -= len(p)
		if j.bytes < 0 {
			j.bytes = 0
		}
	}
	return out
}

// ExpireGaps forwards gap expiration to the reorder buffer and releases
// the accounting the missing packets would have occupied.
func (j *JitterBuffer) ExpireGaps(nowNanos int64) []Gap {
	gaps := j.reorder.ExpireGaps(nowNanos)
	return gaps
}

// Paused reports whether commit is currently withheld.
func (j *JitterBuffer) Paused() bool { return j.paused }

// Bytes returns current buffered payload size.
func (j *JitterBuffer) Bytes() int { return j.bytes }

// SoftLimit returns the current bandwidth-adapted soft budget.
func (j *JitterBuffer) SoftLimit() int { return j.soft }

// HardLimit returns the immutable memory ceiling.
func (j *JitterBuffer) HardLimit() int { return j.hard }
