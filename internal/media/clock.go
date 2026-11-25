// Package media defines the integer-nanosecond time base and common media
// model types shared by every mediapipe module.
package media

import (
	"fmt"
	"math"
	"time"
)

// Timestamp is a point or duration on the pipeline's shared integer
// nanosecond time base. All modules convert container clocks into Timestamp
// before they cross a package boundary.
type Timestamp int64

const (
	Tick        Timestamp = 1
	Microsecond Timestamp = 1_000
	Millisecond Timestamp = 1_000_000
	Second      Timestamp = 1_000_000_000
)

// Timeline origin. Synthetic media starts at zero; containers with a nonzero
// first timestamp are preserved verbatim rather than shifted.
const Origin Timestamp = 0

func FromDuration(d time.Duration) Timestamp { return Timestamp(d) }
func (t Timestamp) Duration() time.Duration  { return time.Duration(t) }
func (t Timestamp) Add(d time.Duration) Timestamp {
	return Timestamp(int64(t) + int64(d))
}
func (t Timestamp) Sub(u Timestamp) Timestamp { return t - u }
func (t Timestamp) After(u Timestamp) bool    { return t > u }
func (t Timestamp) Before(u Timestamp) bool   { return t < u }
func (t Timestamp) ClampZero() Timestamp {
	if t < 0 {
		return 0
	}
	return t
}

func Min(a, b Timestamp) Timestamp {
	if a < b {
		return a
	}
	return b
}

func Max(a, b Timestamp) Timestamp {
	if a > b {
		return a
	}
	return b
}

// FromScale converts a value expressed in ticks of the given timescale into
// nanoseconds without floating point arithmetic. Truncation is deliberate:
// it makes container writes and re-reads bit-for-bit reproducible.
func FromScale(value int64, timescale uint32) Timestamp {
	if timescale == 0 {
		return 0
	}
	scale := int64(timescale)
	neg := value < 0
	if neg {
		if value == math.MinInt64 {
			return Timestamp(math.MinInt64)
		}
		value = -value
	}
	whole := value / scale
	rem := value % scale
	const ns = int64(Second)
	if whole > math.MaxInt64/ns {
		if neg {
			return Timestamp(math.MinInt64)
		}
		return Timestamp(math.MaxInt64)
	}
	total := whole * ns
	// rem < scale <= MaxUint32, so rem*ns fits in int64.
	total += rem * ns / scale
	if neg {
		total = -total
	}
	return Timestamp(total)
}

// ToScale converts nanoseconds to ticks of the given timescale, truncating
// toward zero.
func ToScale(t Timestamp, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	scale := int64(timescale)
	value := int64(t)
	neg := value < 0
	if neg {
		value = -value
	}
	whole := value / int64(Second)
	rem := value % int64(Second)
	if whole > math.MaxInt64/scale {
		if neg {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	total := whole*scale + rem*scale/int64(Second)
	if neg {
		total = -total
	}
	return total
}

// QuantizeDown rounds a timestamp down to a multiple of unit.
func QuantizeDown(t Timestamp, unit Timestamp) Timestamp {
	if unit <= 0 {
		return t
	}
	q := int64(t) / int64(unit)
	if t < 0 && int64(t)%int64(unit) != 0 {
		q--
	}
	return Timestamp(q * int64(unit))
}

// QuantizeNearest rounds a timestamp to the nearest multiple of unit.
func QuantizeNearest(t Timestamp, unit Timestamp) Timestamp {
	if unit <= 0 {
		return t
	}
	down := QuantizeDown(t, unit)
	up := down + unit
	if t-down < up-t {
		return down
	}
	return up
}

// TrackType mirrors the Matroska TrackType element.
type TrackType uint8

const (
	TrackVideo    TrackType = 1
	TrackAudio    TrackType = 2
	TrackComplex  TrackType = 3
	TrackLogo     TrackType = 16
	TrackSubtitle TrackType = 17
	TrackButtons  TrackType = 18
	TrackControl  TrackType = 32
)

func (t TrackType) String() string {
	switch t {
	case TrackVideo:
		return "video"
	case TrackAudio:
		return "audio"
	case TrackSubtitle:
		return "subtitle"
	default:
		return fmt.Sprintf("tracktype(%d)", uint8(t))
	}
}

// Codec is a container-neutral codec identifier. Matroska CodecID strings are
// used as canonical names; mapping rules translate them per target.
type Codec string

const (
	CodecAVC   Codec = "V_MPEG4/ISO/AVC"
	CodecVP9   Codec = "V_VP9"
	CodecVP8   Codec = "V_VP8"
	CodecMPEG2 Codec = "V_MPEG2"
	CodecOpus  Codec = "A_OPUS"
	CodecAAC   Codec = "A_AAC"
	CodecVorbis Codec = "A_VORBIS"
	CodecText  Codec = "S_TEXT/UTF8"
)

func (c Codec) String() string { return string(c) }

// Track is the shared description of one input or output track.
type Track struct {
	ID              uint64
	UID             uint64
	Type            TrackType
	Codec           Codec
	Language        string
	DefaultDuration Timestamp
	Timescale       uint32
	Width           int
	Height          int
	Channels        int
	SampleRate      int
	CodecPrivate    []byte
}

func (t Track) Clone() Track {
	c := t
	c.CodecPrivate = append([]byte(nil), t.CodecPrivate...)
	return c
}

// Packet is one compressed media frame. Data is never decoded or rewritten by
// the pipeline; remuxers copy it verbatim.
type Packet struct {
	Track    uint64
	DTS      Timestamp
	PTS      Timestamp
	Duration Timestamp
	Keyframe bool
	Data     []byte
	// CRC carries an optional source checksum (for example an EBML CRC-32
	// element computed by the fixture generator).
	CRC uint32
	// Sequence is the source read order. It is only used as a deterministic
	// tie breaker when two packets share a decode timestamp.
	Sequence uint64
}

func (p Packet) Clone() Packet {
	p.Data = append([]byte(nil), p.Data...)
	return p
}

// End returns the exclusive end timestamp.
func (p Packet) End() Timestamp { return p.DTS + p.Duration }
