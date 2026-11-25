// Package media contains the shared 90 kHz rational time base and the common
// typed error model used by every pipeline stage.
package media

import (
	"fmt"
	"time"
)

// ClockRate is the common MPEG time base: 90 000 ticks per second.
const ClockRate int64 = 90000

// ClockDeclaration is mandatory for every source. A nil declaration or
// Present=false is rejected instead of assuming a timeline.
type ClockDeclaration struct {
	Present   bool   `json:"present"`
	Timescale uint32 `json:"timescale"`
	OriginPTS int64  `json:"origin_pts"`
}

// DefaultClock returns an explicit 90 kHz declaration.
func DefaultClock() ClockDeclaration {
	return ClockDeclaration{Present: true, Timescale: uint32(ClockRate), OriginPTS: 0}
}

// Validate rejects a missing or incompatible clock declaration.
func (c *ClockDeclaration) Validate(stage Stage) error {
	if c == nil || !c.Present {
		return E(stage, KindMissingClock, -1, fmt.Errorf("clock declaration is missing"))
	}
	if int64(c.Timescale) != ClockRate {
		return E(stage, KindInvalidArgument, -1,
			fmt.Errorf("timescale=%d, want %d", c.Timescale, ClockRate))
	}
	return nil
}

// DurationToTicks converts a wall-clock duration to the 90 kHz base.
func DurationToTicks(d time.Duration) int64 {
	return int64(d) * ClockRate / int64(time.Second)
}

// PCMTicks converts interleaved frame count at sampleRate to 90 kHz ticks.
func PCMTicks(frameCount int64, sampleRate int) int64 {
	if sampleRate <= 0 {
		return 0
	}
	return frameCount * ClockRate / int64(sampleRate)
}

// SegmentDuration computes end-start while tolerating unsigned wrap only when
// the caller explicitly asks for modular RTP expansion.
func SegmentDuration(start, end int64) int64 { return end - start }

// ExpandUint32 extends a wrapping uint32 counter into a monotonic int64.
// wrapped reports whether the step crossed the uint32 modulus. The function
// never changes raw values; callers decide whether a wrap is acceptable.
func ExpandUint32(prev int64, raw uint32) (next int64, wrapped bool) {
	const modulus = int64(1) << 32
	if prev < 0 {
		return int64(raw), false
	}
	prevRaw := uint32(prev % modulus)
	delta := int64(raw) - int64(prevRaw)
	if delta < -modulus/2 {
		delta += modulus
		wrapped = true
	} else if delta > modulus/2 {
		delta -= modulus
	}
	return prev + delta, wrapped
}

// TimelinePoint is one observed timestamp and its source byte offset.
type TimelinePoint struct {
	PTS    int64
	Offset int64
}

// TimelineTracker classifies duplicate, reversed, gapped and wrapping
// timestamps. It does not repair them.
type TimelineTracker struct {
	Stage        Stage
	ExpectedStep int64
	last         *TimelinePoint
	lastExtended int64
	haveExtended bool
}

// Observe checks a monotonic 90 kHz PTS. ExpectedStep > 0 enables gap
// detection. A zero ExpectedStep only enforces non-decreasing PTS.
func (t *TimelineTracker) Observe(pts, offset int64) error {
	if t.last == nil {
		t.last = &TimelinePoint{PTS: pts, Offset: offset}
		return nil
	}
	last := t.last
	switch {
	case pts == last.PTS:
		return E(t.Stage, KindDuplicate, offset,
			fmt.Errorf("duplicate pts=%d after offset=%d", pts, last.Offset)).WithPTS(pts)
	case pts < last.PTS:
		return E(t.Stage, KindOutOfOrder, offset,
			fmt.Errorf("pts moved backward %d -> %d", last.PTS, pts)).WithPTS(pts)
	case t.ExpectedStep > 0 && pts != last.PTS+t.ExpectedStep:
		return E(t.Stage, KindGap, offset,
			fmt.Errorf("expected pts=%d got %d", last.PTS+t.ExpectedStep, pts)).WithPTS(pts)
	}
	t.last = &TimelinePoint{PTS: pts, Offset: offset}
	return nil
}

// ObserveRTPTimestamp uses modular expansion only to distinguish a real
// wraparound from reversal. A wrap is returned as a classified error and the
// raw timeline is not rewritten.
func (t *TimelineTracker) ObserveRTPTimestamp(raw uint32, offset int64) error {
	if !t.haveExtended {
		t.lastExtended = int64(raw)
		t.haveExtended = true
		return nil
	}
	next, wrapped := ExpandUint32(t.lastExtended, raw)
	if wrapped {
		return E(t.Stage, KindWrap, offset,
			fmt.Errorf("RTP timestamp wrapped at raw=%d", raw)).WithPTS(int64(raw))
	}
	if raw == uint32(t.lastExtended) {
		return E(t.Stage, KindDuplicate, offset, fmt.Errorf("duplicate RTP timestamp")).
			WithPTS(int64(raw))
	}
	if next < t.lastExtended {
		return E(t.Stage, KindOutOfOrder, offset,
			fmt.Errorf("RTP timestamp reversed: %d -> %d", t.lastExtended, next)).
			WithPTS(int64(raw))
	}
	t.lastExtended = next
	return nil
}
