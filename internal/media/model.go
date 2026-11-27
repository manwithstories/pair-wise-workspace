// Package media defines the shared rational media time base, frame
// geometry limits and the single error model used across ingest, clock,
// timeline, audio, recovery and render.
package media

import (
	"errors"
	"fmt"
)

type Kind int

const (
	KindInvalid Kind = iota
	KindInput
	KindClock
	KindGraph
	KindCorrupt
	KindConflict
	KindLimit
)

func (k Kind) String() string {
	switch k {
	case KindInput:
		return "input"
	case KindClock:
		return "clock"
	case KindGraph:
		return "graph"
	case KindCorrupt:
		return "corrupt"
	case KindConflict:
		return "conflict"
	case KindLimit:
		return "limit"
	}
	return "invalid"
}

var (
	// ErrInput marks rejected manifests, paths, sizes and hashes.
	ErrInput = errors.New("input rejected")
	// ErrClock marks invalid or ambiguous time-base reasoning.
	ErrClock = errors.New("invalid time base")
	// ErrGraph marks dependency-graph failures.
	ErrGraph = errors.New("invalid dependency graph")
	// ErrCorrupt marks a frame that is present but cannot be decoded.
	ErrCorrupt = errors.New("corrupt frame")
	// ErrConflict marks unsafe output or cache conflicts.
	ErrConflict = errors.New("conflict")
	// ErrLimit marks a bounded-resource violation.
	ErrLimit = errors.New("resource limit")
)

// MediaError carries one error model for every pipeline stage together with
// the track and sequence it concerns, when they are known.
type MediaError struct {
	Kind   Kind
	Op     string
	Track  string
	Seq    int64
	HasSeq bool
	Err    error
}

func (e *MediaError) Error() string {
	where := ""
	if e.Track != "" {
		where = " track=" + e.Track
	}
	if e.HasSeq {
		where += fmt.Sprintf(" seq=%d", e.Seq)
	}
	if e.Err == nil {
		return fmt.Sprintf("%s:%s", e.Kind, where)
	}
	return fmt.Sprintf("%s: %v%s", e.Kind, e.Err, where)
}

func (e *MediaError) Unwrap() error { return e.Err }
func (e *MediaError) Is(target error) bool {
	switch target {
	case ErrInput:
		return e.Kind == KindInput
	case ErrClock:
		return e.Kind == KindClock
	case ErrGraph:
		return e.Kind == KindGraph
	case ErrCorrupt:
		return e.Kind == KindCorrupt
	case ErrConflict:
		return e.Kind == KindConflict
	case ErrLimit:
		return e.Kind == KindLimit
	}
	return errors.Is(e.Err, target)
}

func New(kind Kind, op, track string, seq int64, err error) *MediaError {
	e := &MediaError{Kind: kind, Op: op, Track: track, Seq: seq, HasSeq: seq >= 0, Err: err}
	return e
}

func Newf(kind Kind, op, track string, seq int64, format string, args ...any) *MediaError {
	return New(kind, op, track, seq, fmt.Errorf(format, args...))
}

// Rational is an exact signed rational number in lowest terms. Every
// stage maps time through it; floating point never accumulates.
type Rational struct {
	Num int64
	Den int64
}

func gcdInt64(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		a = 1
	}
	return a
}

// NewRational normalises sign and reduces by the GCD. Den must be nonzero.
func NewRational(num, den int64) Rational {
	if den == 0 {
		den = 1
	}
	if den < 0 {
		num, den = -num, -den
	}
	g := gcdInt64(num, den)
	if g == 0 {
		g = 1
	}
	return Rational{Num: num / g, Den: den / g}
}

func (r Rational) Add(o Rational) Rational {
	return NewRational(r.Num*o.Den+o.Num*r.Den, r.Den*o.Den)
}

func (r Rational) Sub(o Rational) Rational {
	return NewRational(r.Num*o.Den-o.Num*r.Den, r.Den*o.Den)
}

func (r Rational) MulInt(n int64) Rational {
	return NewRational(r.Num*n, r.Den)
}

// Floor returns the greatest integer not greater than r.
func (r Rational) Floor() int64 {
	if r.Num >= 0 {
		return r.Num / r.Den
	}
	return -((-r.Num + r.Den - 1) / r.Den)
}

func (r Rational) Float() float64 { return float64(r.Num) / float64(r.Den) }

func (r Rational) Cmp(o Rational) int {
	l, rr := r.Num*o.Den, o.Num*r.Den
	switch {
	case l < rr:
		return -1
	case l > rr:
		return 1
	}
	return 0
}

func (r Rational) String() string {
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

const (
	MaxTracks  = 3
	MaxCaches  = 2
	MaxWidth   = 4096
	MaxHeight  = 2160
	FrameRate  = 25
	AudioRate  = 48000
	AudioProbe = 1 << 12
)

var (
	// TimeBase is the shared rational output time base: 1/25 s.
	TimeBase = Rational{Num: 1, Den: FrameRate}
	// TickBase is the nominal device tick: 1 µs, i.e. 1000 ns.
	TickBase = Rational{Num: 1000, Den: 1}
)

// FrameTime returns the left edge of output frame index i.
func FrameTime(i int64) Rational { return Rational{Num: i, Den: FrameRate} }

// FrameIndex maps an exact timestamp onto the 25 fps grid with a
// left-closed, right-open rule: [i/25, (i+1)/25) belongs to frame i.
func FrameIndex(t Rational) int64 { return Rational{Num: t.Num * FrameRate, Den: t.Den}.Floor() }
