// Package timeline defines the shared nanosecond time base, timestamp
// unwrapping/drift correction, and the error type carrying byte offsets.
package timeline

import (
	"errors"
	"fmt"
)

const (
	// PTSTicks is the modulus of a 33-bit MPEG-style PTS.
	PTSTicks uint64 = 1 << 33
	PTSMask  uint64 = PTSTicks - 1
	// Timescale is the conventional 90 kHz PTS clock.
	Timescale int64 = 90000
	// MaxDriftPPM is the largest correctable clock skew.
	MaxDriftPPM = 500
	NSPerSecond = int64(1_000_000_000)
)

// Kind classifies an error for process exit-code mapping.
type Kind int

const (
	KindInternal Kind = iota
	KindArgument
	KindUnrecoverable
	KindStorage
)

// Error is the common error model. Offset is an absolute elementary-stream
// byte offset when known; -1 means not applicable.
type Error struct {
	Kind   Kind
	Stage  string
	Index  int
	Offset int64
	Err    error
}

func (e *Error) Error() string {
	where := e.Stage
	if e.Index >= 0 {
		where += fmt.Sprintf(" au=%d", e.Index)
	}
	if e.Offset >= 0 {
		where += fmt.Sprintf(" byte_offset=%d", e.Offset)
	}
	if e.Err != nil {
		return where + ": " + e.Err.Error()
	}
	return where
}

func (e *Error) Unwrap() error { return e.Err }

func New(kind Kind, stage string, index int, offset int64, format string, args ...any) *Error {
	return &Error{
		Kind:   kind,
		Stage:  stage,
		Index:  index,
		Offset: offset,
		Err:    fmt.Errorf(format, args...),
	}
}

func Wrap(kind Kind, stage string, index int, offset int64, err error) *Error {
	if err == nil {
		return nil
	}
	var te *Error
	if errors.As(err, &te) {
		if te.Stage == "" {
			te.Stage = stage
		}
		if index >= 0 && te.Index < 0 {
			te.Index = index
		}
		if offset >= 0 && te.Offset < 0 {
			te.Offset = offset
		}
		return te
	}
	return &Error{Kind: kind, Stage: stage, Index: index, Offset: offset, Err: err}
}

// ExitCode maps error kinds to the CLI's stable exit codes.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var te *Error
	if errors.As(err, &te) {
		switch te.Kind {
		case KindArgument:
			return 2
		case KindUnrecoverable:
			return 3
		case KindStorage, KindInternal:
			return 4
		}
	}
	return 4
}

// Index is the per-access-unit location and timing record.
type Index struct {
	Offset     int64  `json:"offset"`
	Length     int    `json:"length"`
	PTS33      uint64 `json:"pts33"`
	DurationNS int64  `json:"duration_ns"`
	Keyframe   bool   `json:"keyframe"`
}

// Span is an Index after wrap expansion and drift correction.
type Span struct {
	Index         int   `json:"index"`
	Offset        int64 `json:"offset"`
	Length        int   `json:"length"`
	PTSUnwrapped  int64 `json:"pts_unwrapped_ticks"`
	StartNS       int64 `json:"start_ns"`
	DurationNS    int64 `json:"duration_ns"`
	Keyframe      bool  `json:"keyframe"`
	ExpectedEndNS int64 `json:"expected_end_ns"`
}

// PTSExpander reconstructs monotonic PTS across 2^33 wraparound.
type PTSExpander struct {
	started bool
	last    int64
}

func (x *PTSExpander) Expand(pts33 uint64) (int64, error) {
	if pts33 > PTSMask {
		return 0, New(KindArgument, "timeline.pts", -1, -1, "PTS %d exceeds 33 bits", pts33)
	}
	raw := int64(pts33)
	if !x.started {
		x.started = true
		x.last = raw
		return raw, nil
	}
	lastMod := x.last % int64(PTSTicks)
	if lastMod < 0 {
		lastMod += int64(PTSTicks)
	}
	delta := raw - lastMod
	half := int64(PTSTicks / 2)
	if delta > half {
		delta -= int64(PTSTicks)
	} else if delta < -half {
		delta += int64(PTSTicks)
	}
	x.last += delta
	return x.last, nil
}

// TicksToNS converts a 90 kHz timestamp tick count to nanoseconds.
func TicksToNS(ticks int64) int64 {
	if ticks >= 0 {
		return (ticks*NSPerSecond + Timescale/2) / Timescale
	}
	return -((-ticks*NSPerSecond + Timescale/2) / Timescale)
}

// DriftCorrector applies signed ppm correction. Positive ppm means the
// captured clock runs fast relative to media time.
type DriftCorrector struct {
	PPM int
	den int64
}

func NewDriftCorrector(ppm int) (*DriftCorrector, error) {
	if ppm > MaxDriftPPM || ppm < -MaxDriftPPM {
		return nil, New(KindArgument, "timeline.drift", -1, -1,
			"drift %d ppm exceeds correctable limit %d", ppm, MaxDriftPPM)
	}
	return &DriftCorrector{PPM: ppm, den: 1_000_000 + int64(ppm)}, nil
}

func (c *DriftCorrector) Correct(ns int64) int64 {
	if c == nil || c.PPM == 0 {
		return ns
	}
	num := ns * 1_000_000
	if num >= 0 {
		return (num + c.den/2) / c.den
	}
	return -((-num + c.den/2) / c.den)
}

// Build expands wraparound and corrects clock drift while retaining each
// index's own variable-frame duration.
func Build(indexes []Index, driftPPM int) ([]Span, error) {
	corr, err := NewDriftCorrector(driftPPM)
	if err != nil {
		return nil, err
	}
	expander := &PTSExpander{}
	out := make([]Span, 0, len(indexes))
	for i, idx := range indexes {
		if idx.Offset < 0 {
			return nil, New(KindArgument, "timeline.index", i, idx.Offset, "negative offset")
		}
		if idx.Length < 0 {
			return nil, New(KindArgument, "timeline.index", i, idx.Offset, "negative length")
		}
		if idx.DurationNS < 0 {
			return nil, New(KindArgument, "timeline.index", i, idx.Offset, "negative duration")
		}
		ticks, err := expander.Expand(idx.PTS33)
		if err != nil {
			return nil, Wrap(KindArgument, "timeline.pts", i, idx.Offset, err)
		}
		start := corr.Correct(TicksToNS(ticks))
		out = append(out, Span{
			Index:         i,
			Offset:        idx.Offset,
			Length:        idx.Length,
			PTSUnwrapped:  ticks,
			StartNS:       start,
			DurationNS:    idx.DurationNS,
			Keyframe:      idx.Keyframe,
			ExpectedEndNS: start + idx.DurationNS,
		})
	}
	return out, nil
}
