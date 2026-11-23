// Package clock maps per-device capture timestamps onto a single monotonic
// presentation timeline. Devices differ in clock precision, occasionally jump
// backwards, and may wrap their 32-bit counters. Every frame is mapped with
// integer arithmetic only, so the resulting PTS is reproducible byte for byte
// across runs and hosts.
package clock

import (
	"errors"
	"fmt"
)

// ErrBadTimebase reports a Timebase that cannot describe a clock.
var ErrBadTimebase = errors.New("clock: invalid timebase")

// Timebase describes one track's capture clock and the presentation timescale
// it should be expressed in. Timescale is ticks per second of the unified
// timeline (shared by all tracks); CaptureUPS is ticks per second of the
// device's own capture counter. WrapUnits is the modulus of that counter
// (0 means it does not wrap, e.g. a 64-bit counter).
type Timebase struct {
	Timescale  uint32
	CaptureUPS uint32
	WrapUnits  int64
}

// Validate reports whether the timebase is usable.
func (tb Timebase) Validate() error {
	switch {
	case tb.Timescale == 0:
		return fmt.Errorf("%w: timescale is zero", ErrBadTimebase)
	case tb.CaptureUPS == 0:
		return fmt.Errorf("%w: capture rate is zero", ErrBadTimebase)
	case tb.WrapUnits < 0:
		return fmt.Errorf("%w: negative wrap modulus", ErrBadTimebase)
	}
	return nil
}

// UnitsToTicks converts a capture counter value to unified ticks. The
// multiplication is done with 128-bit intermediates so it cannot overflow even
// for wide counters; only the final remainder division truncates.
func (tb Timebase) UnitsToTicks(units int64) int64 {
	ups := int64(tb.CaptureUPS)
	ts := int64(tb.Timescale)
	whole := units / ups
	rem := units % ups
	// rem*ts fits uint64 because rem < ups <= 2^32 and ts <= 2^32.
	frac := int64(uint64(rem) * uint64(ts) / uint64(ups))
	return whole*ts + frac
}

// TickQuantum is the number of ticks in one capture unit, rounded up with a
// floor of one. Two timestamps closer than this cannot be ordered reliably.
func (tb Timebase) TickQuantum() int64 {
	q := (int64(tb.Timescale) + int64(tb.CaptureUPS) - 1) / int64(tb.CaptureUPS)
	if q < 1 {
		q = 1
	}
	return q
}

// Event classifies a repair applied while mapping one frame.
type Event uint8

const (
	// EventNone means the timestamp was already monotonic.
	EventNone Event = iota
	// EventClamp means a backwards jitter smaller than the clock's resolution
	// was pinned to the previous PTS.
	EventClamp
	// EventRollback means a backwards jump larger than tolerance was repaired
	// with a constant offset, preserving the cadence after it.
	EventRollback
	// EventWrap means the capture counter wrapped and the modulus was added.
	EventWrap
	// EventSameTime means the frame shares the previous PTS because two
	// precisions quantised to the same instant. It is not a defect.
	EventSameTime
)

func (e Event) String() string {
	switch e {
	case EventClamp:
		return "clamp"
	case EventRollback:
		return "rollback"
	case EventWrap:
		return "wrap"
	case EventSameTime:
		return "same_time"
	default:
		return "none"
	}
}

// Repairs counts the repairs a Mapper applied. Counters are O(1) state.
type Repairs struct {
	Clamps    int
	Rollbacks int
	Wraps     int
}

// Mapper converts one track's raw capture counter into unified PTS ticks.
// It keeps only constant-size state, so memory does not grow with frame count.
type Mapper struct {
	tb       Timebase
	epoch    int64 // capture units that become PTS 0
	quantum  int64
	seen     bool
	lastRaw  int64 // raw units, before wrap accumulation
	lastAdj  int64 // repaired ticks since epoch
	prevAdj  int64
	lastStep int64
	lastPTS  int64
	wrapAdd  int64 // accumulated wrap modulus in capture units
	offset   int64 // accumulated rollback offset in ticks
	repairs  Repairs
}

// NewMapper validates tb and returns a mapper. Call SetEpochUnits before Map.
func NewMapper(tb Timebase) (*Mapper, error) {
	if err := tb.Validate(); err != nil {
		return nil, err
	}
	return &Mapper{tb: tb, quantum: tb.TickQuantum(), lastStep: tb.TickQuantum()}, nil
}

// SetEpochUnits sets the capture counter value that becomes PTS 0. It must be
// called before the first Map.
func (m *Mapper) SetEpochUnits(units int64) error {
	if m.seen {
		return errors.New("clock: epoch set after mapping started")
	}
	m.epoch = units
	return nil
}

// Map maps one raw capture value to PTS ticks and reports any repair applied.
// A backwards or wrapped stamp is repaired, not rejected; errors are reserved
// for invalid input such as negative counters.
func (m *Mapper) Map(rawUnits int64) (int64, Event, error) {
	if rawUnits < 0 {
		return 0, EventNone, fmt.Errorf("clock: negative capture value %d", rawUnits)
	}
	ev := EventNone
	if m.tb.WrapUnits > 0 && m.seen && rawUnits < m.lastRaw-m.tb.WrapUnits/2 {
		m.wrapAdd += m.tb.WrapUnits
		m.repairs.Wraps++
		ev = EventWrap
	}
	rawTicks := m.tb.UnitsToTicks(rawUnits+m.wrapAdd) + m.offset - m.tb.UnitsToTicks(m.epoch)
	if !m.seen {
		m.seen = true
		m.lastRaw = rawUnits
		m.lastAdj = rawTicks
		m.prevAdj = rawTicks
		m.lastPTS = rawTicks
		return rawTicks, ev, nil
	}

	// A genuine backwards jump is larger than a few capture quanta; a smaller
	// one is quantisation jitter between two precisions and is pinned.
	tolerance := 3 * m.quantum
	if tolerance < 2 {
		tolerance = 2
	}
	if rawTicks < m.lastAdj {
		deficit := m.lastAdj - rawTicks
		if deficit <= tolerance {
			rawTicks = m.lastAdj
			m.repairs.Clamps++
			if ev == EventNone {
				ev = EventClamp
			}
		} else {
			// Shift by the deficit plus one expected step so the jumped frame
			// continues the cadence instead of colliding with the previous PTS.
			m.offset += deficit + m.lastStep
			rawTicks += deficit + m.lastStep
			m.repairs.Rollbacks++
			ev = EventRollback
		}
	}
	if rawTicks < m.lastPTS {
		rawTicks = m.lastPTS
	}
	if rawTicks == m.lastPTS && ev == EventNone {
		ev = EventSameTime
	}
	m.lastStep = rawTicks - m.lastAdj
	if m.lastStep <= 0 {
		m.lastStep = m.quantum
	}
	m.prevAdj = m.lastAdj
	m.lastRaw = rawUnits
	m.lastAdj = rawTicks
	m.lastPTS = rawTicks
	return rawTicks, ev, nil
}

// Repairs returns the accumulated repair counters.
func (m *Mapper) Repairs() Repairs { return m.repairs }

// SeqStatus is the classification of a frame-number sequence step.
type SeqStatus uint8

const (
	// SeqOK is a strictly increasing frame number.
	SeqOK SeqStatus = iota
	// SeqDuplicate repeats the previous number with identical content.
	SeqDuplicate
	// SeqConflict repeats or rewinds the number with different content.
	SeqConflict
	// SeqOutOfOrder rewinds the number; the frame is isolated as a conflict.
	SeqOutOfOrder
)

func (s SeqStatus) String() string {
	switch s {
	case SeqDuplicate:
		return "duplicate"
	case SeqConflict:
		return "conflict"
	case SeqOutOfOrder:
		return "out_of_order"
	default:
		return "ok"
	}
}

// Sequencer detects duplicate and conflicting frame numbers with O(1) state.
// Frames are expected in source order; a number that does not advance is
// isolated, never merged silently.
type Sequencer struct {
	seen     bool
	last     uint64
	lastHash [32]byte
	dup      int
	conflict int
}

// Accept classifies frameNo against the previous frame's number and content
// hash. Duplicate identical frames return SeqDuplicate; any non-advancing
// number with different content returns SeqConflict (or SeqOutOfOrder if the
// number moves backwards).
func (s *Sequencer) Accept(frameNo uint64, hash [32]byte) SeqStatus {
	if !s.seen {
		s.seen, s.last, s.lastHash = true, frameNo, hash
		return SeqOK
	}
	switch {
	case frameNo == s.last && hash == s.lastHash:
		s.dup++
		return SeqDuplicate
	case frameNo == s.last:
		s.conflict++
		return SeqConflict
	case frameNo < s.last:
		s.conflict++
		return SeqOutOfOrder
	}
	s.last, s.lastHash = frameNo, hash
	return SeqOK
}

// Counts returns duplicate and conflict counters.
func (s *Sequencer) Counts() (dup, conflict int) { return s.dup, s.conflict }
