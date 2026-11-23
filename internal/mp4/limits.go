// Package mp4 scans self-generated ISO BMFF byte streams: box hierarchy,
// truncation boundaries, per-sample timing and keyframe spacing. It never
// decodes media payloads; all timing is mapped onto the unified clock grid.
package mp4

import (
	"errors"
	"math"
)

// Limits are hard structural caps that reject hostile or malformed input
// before it can cause unbounded allocation or integer overflow.
type Limits struct {
	MaxFileBytes  int64 // maximum accepted container size
	MaxBoxDepth   int   // maximum box nesting depth
	MaxTracks     int   // maximum number of trak boxes
	MaxBoxes      int   // maximum total box count
	MaxSamples    int64 // maximum samples per track
	MaxQueueLen   int   // passed through to the pipeline layer
	MaxConcurrent int   // passed through to the pipeline layer
}

// DefaultLimits returns conservative caps suitable for local demo fixtures.
func DefaultLimits() Limits {
	return Limits{
		MaxFileBytes:  256 << 20, // 256 MiB
		MaxBoxDepth:   16,
		MaxTracks:     32,
		MaxBoxes:      1 << 20,
		MaxSamples:    1 << 24,
		MaxQueueLen:   4096,
		MaxConcurrent: 8,
	}
}

// Validate rejects nonsensical limit configurations.
func (l Limits) Validate() error {
	if l.MaxFileBytes <= 0 || l.MaxFileBytes > math.MaxInt32 {
		return errors.New("mp4: MaxFileBytes out of range")
	}
	if l.MaxBoxDepth <= 0 || l.MaxBoxDepth > 64 {
		return errors.New("mp4: MaxBoxDepth out of range")
	}
	if l.MaxTracks <= 0 || l.MaxBoxes <= 0 || l.MaxSamples <= 0 {
		return errors.New("mp4: count limits must be positive")
	}
	return nil
}

// BoxType is a four-character ISO BMFF box type.
type BoxType [4]byte

// String renders the box type as its ASCII form.
func (b BoxType) String() string {
	return string(b[:])
}

// Well-known box types (values mirror the go-mp4 StrToBoxType constants).
var (
	BoxFtyp = BoxType{'f', 't', 'y', 'p'}
	BoxMoov = BoxType{'m', 'o', 'o', 'v'}
	BoxTrak = BoxType{'t', 'r', 'a', 'k'}
	BoxMdia = BoxType{'m', 'd', 'i', 'a'}
	BoxMdhd = BoxType{'m', 'd', 'h', 'd'}
	BoxMinf = BoxType{'m', 'i', 'n', 'f'}
	BoxStbl = BoxType{'s', 't', 'b', 'l'}
	BoxStsd = BoxType{'s', 't', 's', 'd'}
	BoxStts = BoxType{'s', 't', 't', 's'}
	BoxStss = BoxType{'s', 't', 's', 's'}
	BoxStsc = BoxType{'s', 't', 's', 'c'}
	BoxStsz = BoxType{'s', 't', 's', 'z'}
	BoxStco = BoxType{'s', 't', 'c', 'o'}
	BoxCo64 = BoxType{'c', 'o', '6', '4'}
	BoxMdat = BoxType{'m', 'd', 'a', 't'}
	BoxFree = BoxType{'f', 'r', 'e', 'e'}
)

// ErrTruncated means a box header or declared body runs past EOF.
var ErrTruncated = errors.New("mp4: truncated box")

// ErrLimit means a structural limit was exceeded.
var ErrLimit = errors.New("mp4: structural limit exceeded")
