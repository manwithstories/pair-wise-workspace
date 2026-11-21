// Package model defines the shared rational media clock and the recoverable
// diagnostic model used by every subsystem. All time math stays integer/rational
// so non-integer frame rates and track offsets cannot accumulate float drift.
package model

import (
	"errors"
	"fmt"
)

// Rational is an exact rational number (Num/Den). Den must always be positive.
type Rational struct {
	Num int64
	Den int64
}

// Common frame rates expressed as exact rationals.
var (
	Rate23976 = Rational{24000, 1001} // 23.976 fps
	Rate2997  = Rational{30000, 1001} // 29.97 fps
	Rate30    = Rational{30, 1}
	Rate60    = Rational{60, 1}
)

// Validate rejects zero/negative denominators and zero numerator overflow.
func (r Rational) Validate() error {
	if r.Den <= 0 {
		return fmt.Errorf("rational %d/%d has non-positive denominator", r.Num, r.Den)
	}
	return nil
}

// Reduce returns the fraction in canonical form (positive denominator).
func (r Rational) Reduce() Rational {
	if err := r.Validate(); err != nil {
		panic(err)
	}
	g := gcd(abs64(r.Num), r.Den)
	if g == 0 {
		return Rational{0, 1}
	}
	return Rational{r.Num / g, r.Den / g}
}

// MulInt scales the fraction by an integer.
func (r Rational) MulInt(v int64) Rational {
	return Rational{r.Num * v, r.Den}.Reduce()
}

// Add returns the exact sum of two rationals.
func (r Rational) Add(o Rational) Rational {
	return Rational{r.Num*o.Den + o.Num*r.Den, r.Den * o.Den}.Reduce()
}

// Sub returns the exact difference of two rationals.
func (r Rational) Sub(o Rational) Rational {
	return Rational{r.Num*o.Den - o.Num*r.Den, r.Den * o.Den}.Reduce()
}

// Cmp returns -1/0/1 like bytes.Compare.
func (r Rational) Cmp(o Rational) int {
	d := r.Num*o.Den - o.Num*r.Den
	switch {
	case d < 0:
		return -1
	case d > 0:
		return 1
	default:
		return 0
	}
}

// Seconds returns the floating-point value in seconds (display only; never used
// for alignment math).
func (r Rational) Seconds() float64 { return float64(r.Num) / float64(r.Den) }

// Millis returns the floating-point value in milliseconds for reporting.
func (r Rational) Millis() float64 { return r.Seconds() * 1000 }

func (r Rational) String() string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Clock is a fixed media time base. A time value t means t/TickRate seconds.
type Clock struct {
	TickRate int64
}

// Well-known RTP media clocks used by the synthetic fixtures.
var (
	Clock90k = Clock{TickRate: 90000} // video
	Clock48k = Clock{TickRate: 48000} // audio
)

// FromSeconds converts an exact duration in seconds to ticks, rounding halves up.
func (c Clock) FromSeconds(d Rational) int64 {
	if err := d.Validate(); err != nil {
		panic(err)
	}
	num := d.Num * c.TickRate
	if num >= 0 {
		return (num + d.Den/2) / d.Den
	}
	return -((-num + d.Den/2) / d.Den)
}

// ToRational converts ticks back to an exact duration in seconds.
func (c Clock) ToRational(ticks int64) Rational {
	return Rational{ticks, c.TickRate}.Reduce()
}

// ToMillis converts ticks to milliseconds for metrics and logs.
func (c Clock) ToMillis(ticks int64) float64 {
	return c.ToRational(ticks).Millis()
}

// Convert translates ticks from another clock into this clock, rounding halves up.
func (c Clock) Convert(ticks int64, from Clock) int64 {
	return c.FromSeconds(from.ToRational(ticks))
}

// Unwrap32 extends a 32-bit RTP timestamp against the previous extended value,
// handling wraparound via the half-range rule.
func Unwrap32(prevExtended int64, current uint32) int64 {
	const half = int64(1) << 31
	delta := int64(current) - (prevExtended & 0xffffffff)
	if delta > half {
		delta -= 1 << 32
	} else if delta < -half {
		delta += 1 << 32
	}
	return prevExtended + delta
}

// Unwrap16 extends a 16-bit RTP sequence number against the previous extended value.
func Unwrap16(prevExtended int64, current uint16) int64 {
	const half = int64(1) << 15
	delta := int64(current) - (prevExtended & 0xffff)
	if delta > half {
		delta -= 1 << 16
	} else if delta < -half {
		delta += 1 << 16
	}
	return prevExtended + delta
}

// TrackKind identifies the media family of a track or frame.
type TrackKind int

const (
	KindVideo TrackKind = iota
	KindAudio
)

func (k TrackKind) String() string {
	if k == KindAudio {
		return "audio"
	}
	return "video"
}

// Frame is the unit that travels from transport recovery into the timeline and
// the offscreen renderer. Timestamp is expressed in Clock ticks.
type Frame struct {
	Track      string
	Kind       TrackKind
	Rep        string // representation (bitrate tier) that contributed the frame
	Seq        int64  // extended RTP sequence number
	Timestamp  int64  // unwrapped RTP timestamp, track clock ticks
	Clock      Clock
	Marker     bool
	Payload    []byte
	Recovered  bool  // arrived late but inside the reorder window
	Incomplete bool  // unrepairable gap placeholder; Payload is nil
	SendTicks  int64 // shared-clock tick when the packet was injected
	RecvTicks  int64 // shared-clock tick when it became available
}

// Diagnostic kinds shared across subsystems.
type Kind string

const (
	KindBadRotation      Kind = "bad_rotation"
	KindCropOutOfBounds  Kind = "crop_out_of_bounds"
	KindColorConflict    Kind = "color_conflict"
	KindMissingField     Kind = "missing_field"
	KindIllegalField     Kind = "illegal_field"
	KindUnalignable      Kind = "unalignable"
	KindPacketGap        Kind = "packet_gap"
	KindDuplicatePacket  Kind = "duplicate_packet"
	KindBoundaryMismatch Kind = "boundary_mismatch"
	KindMissingFrame     Kind = "missing_frame"
	KindFrameSuppressed  Kind = "frame_suppressed"
)

// Severity ranks diagnostics for reporting.
type Severity int

const (
	SeverityInfo Severity = iota
	SeverityWarning
	SeverityError
)

// Diagnostic is the common, serializable error model. Every structural problem
// carries the track, a shared-clock timestamp and a recoverability verdict.
type Diagnostic struct {
	Kind        Kind
	Severity    Severity
	Track       string
	AtTicks     int64 // -1 when no timestamp applies
	Recoverable bool
	Message     string
}

// Error implements the error interface.
func (d Diagnostic) Error() string {
	flag := "fatal"
	if d.Recoverable {
		flag = "recoverable"
	}
	return fmt.Sprintf("%s: track=%s ticks=%d %s: %s", d.Kind, d.Track, d.AtTicks, flag, d.Message)
}

// NoTimestamp is the sentinel for diagnostics without a media timestamp.
const NoTimestamp int64 = -1

// ErrIncomplete is returned/wrapped when a clip cannot be fully aligned.
var ErrIncomplete = errors.New("incomplete track segment")

// Bag is an append-only, ordered collection of diagnostics.
type Bag struct {
	items []Diagnostic
}

// Add records one diagnostic and returns the bag for chaining.
func (b *Bag) Add(d Diagnostic) *Bag {
	if d.AtTicks == 0 {
		d.AtTicks = NoTimestamp
	}
	b.items = append(b.items, d)
	return b
}

// All returns the collected diagnostics in insertion order.
func (b *Bag) All() []Diagnostic {
	out := make([]Diagnostic, len(b.items))
	copy(out, b.items)
	return out
}

// Count returns how many diagnostics of the given kind were recorded.
func (b *Bag) Count(k Kind) int {
	n := 0
	for _, d := range b.items {
		if d.Kind == k {
			n++
		}
	}
	return n
}

// Fatal returns the diagnostics that were marked unrecoverable.
func (b *Bag) Fatal() []Diagnostic {
	var out []Diagnostic
	for _, d := range b.items {
		if !d.Recoverable {
			out = append(out, d)
		}
	}
	return out
}

// Len returns the number of collected diagnostics.
func (b *Bag) Len() int { return len(b.items) }
