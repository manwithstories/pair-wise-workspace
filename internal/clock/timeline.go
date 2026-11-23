// Package clock defines the shared rational media time base used to map
// per-track timestamps (with non-zero starts and edit lists) onto one
// archive-wide timeline. All arithmetic is exact rational math; any
// overflow is reported, never silently truncated.
package clock

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// BaseTimescale is the archive-wide shared time base (90 kHz, the RTP clock).
const BaseTimescale int64 = 90000

// ErrOverflow is returned when a timestamp conversion would overflow int64.
var ErrOverflow = errors.New("clock: timestamp overflow")

// Time is an exact rational number of seconds: Num/Den.
type Time struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

func gcd(a, b int64) int64 {
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
		return 1
	}
	return a
}

// New builds a normalized Time. den must be positive.
func New(num, den int64) (Time, error) {
	if den <= 0 {
		return Time{}, fmt.Errorf("clock: invalid denominator %d", den)
	}
	g := gcd(num, den)
	return Time{Num: num / g, Den: den / g}, nil
}

// Must is New for compile-time constants; panics on invalid input.
func Must(num, den int64) Time {
	t, err := New(num, den)
	if err != nil {
		panic(err)
	}
	return t
}

// Zero is the timeline origin.
var Zero = Time{Num: 0, Den: 1}

// Cmp compares two times: -1, 0 or +1. Uses 128-bit products to stay exact.
func (t Time) Cmp(o Time) int {
	hi1, lo1 := bits.Mul64(uint64(t.Num), uint64(o.Den))
	hi2, lo2 := bits.Mul64(uint64(o.Num), uint64(t.Den))
	// signs: Num may be negative; Den always positive. Compare via big-free path:
	if sign(t.Num) != sign(o.Num) {
		return cmpInt(sign(t.Num), sign(o.Num))
	}
	if hi1 != hi2 {
		return cmpUint(hi1, hi2)
	}
	return cmpUint(lo1, lo2)
}

func sign(v int64) int64 {
	if v < 0 {
		return -1
	}
	if v > 0 {
		return 1
	}
	return 0
}
func cmpInt(a, b int64) int   { return int(sign(a - b)) }
func cmpUint(a, b uint64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// Add returns t+o as an exact rational.
func (t Time) Add(o Time) (Time, error) {
	// num = t.Num*o.Den + o.Num*t.Den ; den = t.Den*o.Den
	den, ok := mulChecked(t.Den, o.Den)
	if !ok {
		return Time{}, ErrOverflow
	}
	a, ok1 := mulChecked(t.Num, o.Den)
	b, ok2 := mulChecked(o.Num, t.Den)
	if !ok1 || !ok2 {
		return Time{}, ErrOverflow
	}
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return Time{}, ErrOverflow
	}
	return New(a+b, den)
}

// Sub returns t-o.
func (t Time) Sub(o Time) (Time, error) { return t.Add(Time{Num: -o.Num, Den: o.Den}) }

// Millis returns the time truncated toward zero in milliseconds.
func (t Time) Millis() int64 { return t.Num * 1000 / t.Den }

// FloatSeconds returns a float approximation (display only).
func (t Time) FloatSeconds() float64 { return float64(t.Num) / float64(t.Den) }

func mulChecked(a, b int64) (int64, bool) {
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	// sign-extension check: hi must equal lo's sign bit replicated,
	// and the signed product must fit.
	p := int64(lo)
	if a != 0 && (p/a != b || (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64)) {
		return 0, false
	}
	_ = hi
	return p, true
}

// ToBase converts a media timestamp (in the given track timescale) to an
// integer count on the shared 90 kHz base. Overflow is an error, never a
// silent truncation.
func ToBase(media, timescale int64) (int64, error) {
	if timescale <= 0 {
		return 0, fmt.Errorf("clock: invalid timescale %d", timescale)
	}
	num, ok := mulChecked(media, BaseTimescale)
	if !ok {
		return 0, ErrOverflow
	}
	return num / timescale, nil
}

// Edit is one elst entry. Empty edits (MediaTime < 0) insert presentation
// time without consuming media. Duration is in movie timescale units.
type Edit struct {
	MediaTime int64 `json:"media_time"`
	Duration  int64 `json:"duration"`
	Empty     bool  `json:"empty"`
}

// Timeline maps track media timestamps onto the shared presentation line.
type Timeline struct {
	Timescale      int64  `json:"timescale"`
	MovieTimescale int64  `json:"movie_timescale"`
	Edits          []Edit `json:"edits"`
}

// NewTimeline validates the edit list. Overlapping media intervals are
// rejected with the index of the first offending edit.
func NewTimeline(timescale, movieTimescale int64, edits []Edit) (*Timeline, error) {
	if timescale <= 0 || movieTimescale <= 0 {
		return nil, fmt.Errorf("clock: invalid timescale %d/%d", timescale, movieTimescale)
	}
	for i, e := range edits {
		if e.Empty {
			continue
		}
		if e.MediaTime < 0 {
			return nil, fmt.Errorf("clock: edit %d has negative media time %d", i, e.MediaTime)
		}
		// media interval covered by this edit, in media units:
		// [MediaTime, MediaTime + Duration*timescale/movieTimescale)
		end, err := editMediaEnd(e, timescale, movieTimescale)
		if err != nil {
			return nil, err
		}
		for j := i + 1; j < len(edits); j++ {
			n := edits[j]
			if n.Empty {
				continue
			}
			if n.MediaTime < end && n.MediaTime >= e.MediaTime {
				return nil, fmt.Errorf("clock: edit %d overlaps edit %d (media_time %d inside [%d,%d))",
					j, i, n.MediaTime, e.MediaTime, end)
			}
			break // only the next non-empty edit can overlap
		}
	}
	return &Timeline{Timescale: timescale, MovieTimescale: movieTimescale, Edits: edits}, nil
}

func editMediaEnd(e Edit, timescale, movieTimescale int64) (int64, error) {
	d, ok := mulChecked(e.Duration, timescale)
	if !ok {
		return 0, ErrOverflow
	}
	end := d / movieTimescale
	if e.MediaTime > math.MaxInt64-end {
		return 0, ErrOverflow
	}
	return e.MediaTime + end, nil
}

// PresentationOffset returns the presentation time assigned to media time 0
// (non-zero for assets whose first edit starts later, or whose initial edit
// is an empty edit inserting a delay).
func (tl *Timeline) PresentationOffset() (Time, error) {
	cursor := Zero
	for _, e := range tl.Edits {
		d, err := New(e.Duration, tl.MovieTimescale)
		if err != nil {
			return Time{}, err
		}
		if e.Empty {
			if cursor, err = cursor.Add(d); err != nil {
				return Time{}, err
			}
			continue
		}
		// media time 0 maps inside/before this edit's media interval.
		mt, err := New(e.MediaTime, tl.Timescale)
		if err != nil {
			return Time{}, err
		}
		return cursor.Sub(mt)
	}
	return Zero, nil // no edits: identity
}

// ToPresentation maps a media timestamp to the shared presentation line.
func (tl *Timeline) ToPresentation(media int64) (Time, error) {
	if len(tl.Edits) == 0 {
		return New(media, tl.Timescale)
	}
	cursor := Zero
	for _, e := range tl.Edits {
		d, err := New(e.Duration, tl.MovieTimescale)
		if err != nil {
			return Time{}, err
		}
		if e.Empty {
			if cursor, err = cursor.Add(d); err != nil {
				return Time{}, err
			}
			continue
		}
		end, err := editMediaEnd(e, tl.Timescale, tl.MovieTimescale)
		if err != nil {
			return Time{}, err
		}
		if media < end || e == tl.Edits[len(tl.Edits)-1] {
			mt, err := New(media-e.MediaTime, tl.Timescale)
			if err != nil {
				return Time{}, err
			}
			return cursor.Add(mt)
		}
		if cursor, err = cursor.Add(d); err != nil {
			return Time{}, err
		}
	}
	return New(media, tl.Timescale)
}
