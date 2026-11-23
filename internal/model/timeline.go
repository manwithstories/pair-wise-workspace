// Package model defines the archive's shared timeline model and error contract.
// Every subsystem reports defects with the same stable Code, an optional
// location (manifest line, track, sample, byte offset) and the range of work
// that can still be recovered.
package model

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"time"
)

// Code is a stable, machine-readable error identifier. Codes never change
// meaning between releases; diagnostics are keyed by them.
type Code string

const (
	CodeManifestReference  Code = "manifest_invalid_reference"
	CodeManifestSyntax     Code = "manifest_syntax"
	CodeManifestDigest     Code = "manifest_digest_mismatch"
	CodeEditListRange      Code = "editlist_out_of_range"
	CodeTruncatedSample    Code = "truncated_sample"
	CodeCorruptKeyTable    Code = "corrupt_keyframe_table"
	CodeTimescaleOverflow  Code = "timescale_overflow"
	CodeNoDecodePoint      Code = "no_decodable_point"
	CodeTransportDefect    Code = "transport_defect"
	CodeSourceCorrupt      Code = "source_corrupt"
	CodeCacheMismatch      Code = "cache_mismatch"
	CodeCacheMiss          Code = "cache_miss"
	CodeCheckpointInvalid  Code = "checkpoint_invalid"
	CodeCheckpointMismatch Code = "checkpoint_mismatch"
	CodeInternal           Code = "internal_error"
)

// Recoverable describes the portion of the input that remains usable after a
// defect. Byte offsets are inclusive-start/exclusive-end; sample indices are
// half-open [FirstSample, LastSample).
type Recoverable struct {
	FirstSample int
	LastSample  int
	FirstByte   int64
	EndByte     int64
}

// Error is the single diagnostic type shared by all subsystems.
type Error struct {
	Code        Code
	Detail      string
	File        string
	Line        int // 1-based manifest line, 0 when not applicable
	Track       uint32
	Sample      int   // 0-based sample index, -1 when not applicable
	OffsetBytes int64 // file byte offset, -1 when not applicable
	Recoverable *Recoverable
	Err         error
}

func (e *Error) Error() string {
	msg := string(e.Code)
	if e.File != "" {
		msg += " in " + e.File
	}
	if e.Line > 0 {
		msg += fmt.Sprintf(" line %d", e.Line)
	}
	if e.Track != 0 {
		msg += fmt.Sprintf(" track %d", e.Track)
	}
	if e.Sample >= 0 {
		msg += fmt.Sprintf(" sample %d", e.Sample)
	}
	if e.OffsetBytes >= 0 {
		msg += fmt.Sprintf(" offset %d", e.OffsetBytes)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// CodeOf returns the stable code of the first *Error in the unwrap chain.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// NewError builds a diagnostic with sane defaults for optional fields.
func NewError(code Code, detail string) *Error {
	return &Error{Code: code, Detail: detail, Sample: -1, OffsetBytes: -1}
}

// UnifiedTimescale is the common clock used for cross-track ordering and
// random access. One tick is one microsecond, which exactly represents the
// 90 kHz and 48 kHz media clocks used by the demo material.
const UnifiedTimescale uint32 = 1_000_000

// Timebase maps one track's media clock onto the unified timeline while
// retaining the source clock and its non-zero origin.
type Timebase struct {
	TrackID     uint32
	Timescale   uint32 // media ticks per second
	OriginTicks int64  // source presentation time of the first editable sample
}

// Validate rejects clocks that cannot be represented safely.
func (tb Timebase) Validate() error {
	if tb.Timescale == 0 {
		return &Error{Code: CodeTimescaleOverflow, Track: tb.TrackID, Sample: -1, OffsetBytes: -1,
			Detail: "track timescale is zero"}
	}
	return nil
}

// Scale converts ticks from one clock to another using 128-bit intermediate
// arithmetic. It reports false instead of overflowing.
func Scale(ticks int64, from, to uint32) (int64, bool) {
	if from == 0 {
		return 0, false
	}
	if from == to {
		return ticks, true // identity, including MinInt64
	}
	neg := ticks < 0
	var mag uint64
	if neg {
		mag = uint64(-(ticks + 1)) + 1
	} else {
		mag = uint64(ticks)
	}
	hi, lo := bits.Mul64(mag, uint64(to))
	if hi >= uint64(from) {
		return 0, false // quotient would not fit in 64 bits
	}
	q, _ := bits.Div64(hi, lo, uint64(from))
	var lim uint64 = math.MaxInt64
	if neg {
		lim = 1 << 63
	}
	if q > lim {
		return 0, false
	}
	if neg {
		return -int64(q), true
	}
	return int64(q), true
}

// ToUnified maps a media tick onto the unified microsecond clock.
//
// It is exact when the media clock divides evenly into microseconds, i.e.
// when 1_000_000/Timescale is an integer (90 kHz video and 48 kHz audio both
// qualify: 90 Hz and 48 Hz respectively). Otherwise it falls back to
// truncated scaling, which is monotonically non-decreasing.
func (tb Timebase) ToUnified(mediaTick int64) (int64, error) {
	if tb.Timescale == 0 {
		return 0, &Error{Code: CodeTimescaleOverflow, Track: tb.TrackID, Sample: -1, OffsetBytes: -1,
			Detail: "track timescale is zero"}
	}
	if factor64 := int64(UnifiedTimescale) / int64(tb.Timescale); factor64 > 0 &&
		int64(tb.Timescale)*factor64 == int64(UnifiedTimescale) {
		sign := int64(1)
		mag := mediaTick
		if mag < 0 {
			sign = -1
			mag = -mag
		}
		if mag > math.MaxInt64/factor64 {
			return 0, &Error{Code: CodeTimescaleOverflow, Track: tb.TrackID, Sample: -1, OffsetBytes: -1,
				Detail: "media tick overflows the unified timeline"}
		}
		return sign * mag * factor64, nil
	}
	tick, ok := Scale(mediaTick, tb.Timescale, UnifiedTimescale)
	if !ok {
		return 0, &Error{Code: CodeTimescaleOverflow, Track: tb.TrackID, Sample: -1, OffsetBytes: -1,
			Detail: "media tick overflows the unified timeline"}
	}
	return tick, nil
}

// ToDuration maps a unified tick count to wall-clock duration.
func ToDuration(unifiedTick int64) (time.Duration, bool) {
	if unifiedTick > math.MaxInt64/int64(time.Millisecond) || unifiedTick < math.MinInt64/int64(time.Millisecond) {
		return 0, false
	}
	return time.Duration(unifiedTick) * time.Microsecond, true
}

// FromDuration converts wall-clock duration to unified ticks.
func FromDuration(d time.Duration) int64 {
	return int64(d / time.Microsecond)
}

// Timeline is the shared clock handed from manifest parsing to repair and
// indexing.
type Timeline struct {
	Timescale uint32
	tracks    map[uint32]Timebase
	ordered   []Timebase
}

// NewTimeline validates every track clock and returns an immutable timeline.
func NewTimeline(tbs ...Timebase) (*Timeline, error) {
	tl := &Timeline{Timescale: UnifiedTimescale, tracks: make(map[uint32]Timebase, len(tbs))}
	for _, tb := range tbs {
		if err := tb.Validate(); err != nil {
			return nil, err
		}
		if tb.TrackID == 0 {
			return nil, &Error{Code: CodeManifestReference, Track: 0, Sample: -1, OffsetBytes: -1,
				Detail: "track id 0 is reserved"}
		}
		if _, dup := tl.tracks[tb.TrackID]; dup {
			return nil, &Error{Code: CodeManifestReference, Track: tb.TrackID, Sample: -1, OffsetBytes: -1,
				Detail: "duplicate track id"}
		}
		tl.tracks[tb.TrackID] = tb
		tl.ordered = append(tl.ordered, tb)
	}
	return tl, nil
}

// Base returns the timebase for a track.
func (tl *Timeline) Base(trackID uint32) (Timebase, bool) {
	tb, ok := tl.tracks[trackID]
	return tb, ok
}

// Tracks returns the registered timebases in input order.
func (tl *Timeline) Tracks() []Timebase { return tl.ordered }

// Unified maps a track's media tick onto the shared clock.
func (tl *Timeline) Unified(trackID uint32, mediaTick int64) (int64, error) {
	tb, ok := tl.tracks[trackID]
	if !ok {
		return 0, &Error{Code: CodeManifestReference, Track: trackID, Sample: -1, OffsetBytes: -1,
			Detail: "track is not part of the timeline"}
	}
	return tb.ToUnified(mediaTick)
}
