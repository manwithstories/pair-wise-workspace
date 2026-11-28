// Package media defines the unified nanosecond timebase model and the
// Fault error type shared by every pipeline stage.
package media

import "fmt"

// Unified timebase constants. All packet timestamps are converted to
// nanoseconds on a single reference timeline before merging.
const (
	NanosPerSecond = int64(1_000_000_000)
	// AudioRate is the PCM sample rate of the supported audio subset.
	AudioRate = int64(48_000)
	// VideoClock is the clock rate of 33-bit MPEG-style PTS values.
	VideoClock = int64(90_000)
	// MaxFileSize bounds any input file we will slurp, to cap memory.
	MaxFileSize = 64 << 20
	// MaxPackets bounds the packet count accepted from one source.
	MaxPackets = 1 << 20
)

// Kind classifies a packet on the merged timeline.
type Kind int

const (
	KindVideo Kind = iota
	KindAudio
	KindParity
)

func (k Kind) String() string {
	switch k {
	case KindVideo:
		return "video"
	case KindAudio:
		return "audio"
	case KindParity:
		return "parity"
	}
	return "unknown"
}

// Stage identifies the pipeline stage that raised a Fault.
type Stage string

const (
	StageParse     Stage = "parse"
	StageCalibrate Stage = "calibrate"
	StageUnwrap    Stage = "unwrap"
	StageRecover   Stage = "recover"
	StageNormalize Stage = "normalize"
	StageMerge     Stage = "merge"
	StageRender    Stage = "render"
)

// Fault is a structured, source-locatable error. Track and Offset identify
// the exact track and byte offset in the source that caused the failure.
type Fault struct {
	Stage  Stage
	Track  int
	Offset int64
	Msg    string
}

func (f *Fault) Error() string {
	return fmt.Sprintf("fault stage=%s track=%d offset=%d: %s", f.Stage, f.Track, f.Offset, f.Msg)
}

// Faultf builds a Fault with a formatted message.
func Faultf(stage Stage, track int, offset int64, format string, args ...any) *Fault {
	return &Fault{Stage: stage, Track: track, Offset: offset, Msg: fmt.Sprintf(format, args...)}
}

// AsFault extracts a *Fault from an error chain.
func AsFault(err error) (*Fault, bool) {
	for err != nil {
		if f, ok := err.(*Fault); ok {
			return f, true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return nil, false
}

// Anchor pairs a local (device clock) timestamp with the reference
// timeline, both in nanoseconds. Used for rational drift fitting.
type Anchor struct {
	Local int64
	Ref   int64
}

// Packet is one parsed essence unit carrying its provenance.
type Packet struct {
	Track     int    // source track number
	Seq       int64  // per-track packet sequence
	Kind      Kind   // essence kind
	PTS       int64  // nanoseconds on the unified reference timeline
	Duration  int64  // nanoseconds
	Offset    int64  // byte offset inside Source
	Source    string // source file base name
	Data      []byte // JPEG frame or PCM payload
	Lost      bool   // placeholder for an unrecovered gap
	Recovered bool   // payload rebuilt from parity
}

// TicksToNS converts a count of clock ticks to nanoseconds, rounding to
// nearest. rate must be positive.
func TicksToNS(ticks, rate int64) int64 {
	if rate <= 0 {
		return 0
	}
	// ticks is bounded well below 1<<62 by MaxFileSize, so ticks*1e9
	// cannot overflow int64 for realistic inputs; guard anyway.
	const maxSafe = (1<<62 - 1) / NanosPerSecond
	if ticks > maxSafe {
		ticks = maxSafe
	}
	num := ticks * NanosPerSecond
	if num >= 0 {
		return (num + rate/2) / rate
	}
	return (num - rate/2) / rate
}
