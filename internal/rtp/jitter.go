package rtp

import (
	"sort"

	"mediasim/internal/clock"
)

// JitterOptions bounds the playout buffer.
type JitterOptions struct {
	// TargetDelayNanos is the playout delay; arrivals older than this
	// relative to the newest packet are expired.
	TargetDelayNanos int64
	// MaxPackets caps buffered packets. On overflow the oldest packet is
	// released immediately so memory never grows without bound.
	MaxPackets int
}

// JitterBuffer holds out-of-order arrivals until their playout deadline and
// tracks whether the decoder must wait for the next keyframe.
type JitterBuffer struct {
	opt      JitterOptions
	buf      []Packet
	newest   int64
	hasClock bool
	waiting  bool
	stats    JitterStats
	// lastFrame is the highest frame index already played, so a late
	// packet behind it is dropped instead of rewinding the video.
	lastFrame int
	sawFrame  bool
}

// JitterStats reports buffer behaviour.
type JitterStats struct {
	Inserted      int `json:"inserted"`
	Played        int `json:"played"`
	Expired       int `json:"expired"`
	Dropped       int `json:"dropped"`
	KeyframeWaits int `json:"keyframe_waits"`
	MaxDepth      int `json:"max_depth"`
}

// NewJitterBuffer validates the bounds.
func NewJitterBuffer(opt JitterOptions) (*JitterBuffer, error) {
	if opt.TargetDelayNanos < 0 {
		return nil, clock.Errorf(clock.CatArgument, clock.Span{}, clock.RecoveredNone, "negative target delay")
	}
	if opt.MaxPackets <= 0 || opt.MaxPackets > 4096 {
		return nil, clock.Errorf(clock.CatArgument, clock.Span{}, clock.RecoveredNone,
			"max packets %d outside 1..4096", opt.MaxPackets)
	}
	return &JitterBuffer{opt: opt, lastFrame: -1}, nil
}

// Insert accepts a packet arriving at arrivalNanos. It returns packets whose
// playout deadline has passed together with the errors that were handled.
func (j *JitterBuffer) Insert(p Packet, arrivalNanos int64) ([]Packet, []error) {
	var errs []error
	if !j.hasClock || arrivalNanos > j.newest {
		j.newest = arrivalNanos
		j.hasClock = true
	}
	// A packet at or behind the last played frame can never be used.
	if j.sawFrame && p.Frame <= j.lastFrame {
		j.stats.Dropped++
		errs = append(errs, clock.Errorf(clock.CatTransport,
			clock.NewSpan("", "", clock.Interval{Start: int64(p.Frame), End: int64(p.Frame) + 1}),
			clock.RecoveredDropPacket, "packet for frame %d arrived after playout", p.Frame))
		return nil, errs
	}
	// Hard cap: if deadlines could not drain the buffer, release the oldest
	// packet so memory stays bounded by MaxPackets.
	var out []Packet
	for len(j.buf)+1 > j.opt.MaxPackets {
		if len(j.buf) == 0 {
			break
		}
		oldest := j.takeOldest()
		j.stats.Expired++
		errs = append(errs, clock.Errorf(clock.CatTransport, spanFor(oldest),
			clock.RecoveredDropPacket, "jitter buffer full; releasing oldest frame %d", oldest.Frame))
		j.play(oldest)
		out = append(out, oldest)
	}
	// Release every packet whose deadline has passed relative to the newest
	// arrival, then admit the new one. Deadline playout is what keeps the
	// buffer bounded in normal operation rather than an overflow path.
	released, perrs := j.Playout()
	out = append(out, released...)
	errs = append(errs, perrs...)
	j.buf = append(j.buf, p)
	j.stats.Inserted++
	if len(j.buf) > j.stats.MaxDepth {
		j.stats.MaxDepth = len(j.buf)
	}
	return out, errs
}

// Playout releases every packet whose deadline is older than newest-delay,
// in frame order. It also closes gaps: if the released run skips frames, the
// buffer enters keyframe-wait state and reports it.
func (j *JitterBuffer) Playout() ([]Packet, []error) {
	if !j.hasClock {
		return nil, nil
	}
	cutoff := j.newest - j.opt.TargetDelayNanos
	sort.SliceStable(j.buf, func(a, b int) bool {
		if j.buf[a].Frame != j.buf[b].Frame {
			return j.buf[a].Frame < j.buf[b].Frame
		}
		return j.buf[a].Seq < j.buf[b].Seq
	})
	var out []Packet
	var errs []error
	idx := 0
	for idx < len(j.buf) && j.buf[idx].LossArrival <= cutoff {
		idx++
	}
	ready := j.buf[:idx]
	j.buf = append([]Packet(nil), j.buf[idx:]...)
	for _, p := range ready {
		if j.sawFrame && p.Frame > j.lastFrame+1 {
			errs = append(errs, j.requestKeyframe(p.Frame))
		}
		j.play(p)
		out = append(out, p)
	}
	return out, errs
}

// Flush releases everything left at end of stream, including anything the
// final deadline playout just released: dropping those would silently lose
// the tail of the stream.
func (j *JitterBuffer) Flush() []Packet {
	released, _ := j.Playout()
	out := append([]Packet(nil), released...)
	out = append(out, j.buf...)
	sort.SliceStable(out, func(a, b int) bool { return out[a].Frame < out[b].Frame })
	j.buf = nil
	for _, p := range out {
		j.play(p)
	}
	return out
}

// WaitingForKeyframe reports whether the decoder is holding for a keyframe.
func (j *JitterBuffer) WaitingForKeyframe() bool { return j.waiting }

// NoteKeyframe clears the wait when a keyframe actually arrives.
func (j *JitterBuffer) NoteKeyframe(frame int) {
	if j.waiting && frame >= j.lastFrame {
		j.waiting = false
	}
}

func (j *JitterBuffer) play(p Packet) {
	j.stats.Played++
	if p.Keyframe {
		j.NoteKeyframe(p.Frame)
	}
	if !j.sawFrame || p.Frame > j.lastFrame {
		j.lastFrame = p.Frame
		j.sawFrame = true
	}
}

func (j *JitterBuffer) requestKeyframe(after int) error {
	j.waiting = true
	j.stats.KeyframeWaits++
	return clock.Errorf(clock.CatTransport,
		clock.NewSpan("", "", clock.Interval{Start: int64(after), End: int64(after) + 1}),
		clock.RecoveredKeyframeReq, "gap before frame %d; requesting keyframe", after)
}

func (j *JitterBuffer) takeOldest() Packet {
	best := 0
	for i := range j.buf {
		if j.buf[i].LossArrival < j.buf[best].LossArrival {
			best = i
		}
	}
	p := j.buf[best]
	j.buf = append(j.buf[:best], j.buf[best+1:]...)
	return p
}

// Depth is the current buffer occupancy.
func (j *JitterBuffer) Depth() int { return len(j.buf) }

// Stats snapshots counters.
func (j *JitterBuffer) Stats() JitterStats { return j.stats }

func spanFor(p Packet) clock.Span {
	return clock.NewSpan("", "", clock.Interval{Start: int64(p.Frame), End: int64(p.Frame) + 1})
}
