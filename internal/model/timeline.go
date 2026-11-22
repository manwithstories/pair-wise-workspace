// Package model defines the shared nanosecond time base, track model and clip
// descriptions used by the parser, scanner and batch subsystems.
package model

import "errors"

// MaxTracks and MaxSamples bound the input size so a malformed container cannot
// make the analyzer allocate unbounded memory.
const (
	MaxTracks  = 16
	MaxSamples = 200_000
)

// TrackKind identifies the role of a track in the multi-track review timeline.
type TrackKind uint8

const (
	KindUnknown TrackKind = iota
	KindVideo
	KindAudio
	KindMarkers // timed tactical-marker track (text/meta samples)
)

func (k TrackKind) String() string {
	switch k {
	case KindVideo:
		return "video"
	case KindAudio:
		return "audio"
	case KindMarkers:
		return "markers"
	default:
		return "unknown"
	}
}

// Track describes one parsed container track in the unified time base.
type Track struct {
	ID        uint32 // track_id from tkhd
	Kind      TrackKind
	Timescale uint32        // mdhd timescale, units per second
	Duration  uint64        // mdhd duration in timescale units
	Samples   []SampleEvent // projected onto the nanosecond axis
	// SyncSamples holds 1-based sample numbers of key frames (stss). A nil
	// slice means no stss box was present.
	SyncSamples []uint32
	// Edit entries (elst) in movie timescale; empty means no edit list.
	Edits []EditEntry
	// Original sample-entry metadata, preserved verbatim.
	Audio AudioMeta
	Video VideoMeta
	// ParseFaults are faults located inside this track's boxes.
	ParseFaults []Fault
}

// SampleEvent is one container sample projected onto the nanosecond time base.
type SampleEvent struct {
	TrackID    uint32
	Index      uint32 // 0-based sample number within the track
	StartNS    int64  // presentation start in unified nanoseconds
	DurationNS int64  // presentation duration in nanoseconds
	Offset     uint64 // byte offset of the sample inside the input file
	Size       uint32 // declared sample size in bytes
	IsSync     bool   // key frame (video only)
}

// EditEntry mirrors one elst entry; times are kept in the movie timescale.
type EditEntry struct {
	SegmentDuration uint64
	MediaTime       int64 // -1 means empty edit
	MediaRate       int32 // fixed-point 16.16
}

// AudioMeta preserves the original audio sample-entry values.
type AudioMeta struct {
	ChannelCount uint16
	SampleRate   uint32 // fixed-point 16.16 as stored
	SampleSize   uint16
	Present      bool
}

// VideoMeta preserves the original visual sample-entry and colr values.
type VideoMeta struct {
	Width                   uint16
	Height                  uint16
	HasColr                 bool
	ColourType              string // "nclx" / "nclc" / ""
	ColourPrimaries         uint16
	TransferCharacteristics uint16
	MatrixCoefficients      uint16
	FullRange               bool
	Present                 bool
}

// ClipPoint is one cut or transition boundary expressed by the operator.
type ClipPoint struct {
	Label   string
	TrackID uint32 // 0 means the point applies to the whole timeline
	AtNS    int64  // requested position on the unified nanosecond axis
}

// Timeline is the parsed input projected onto one nanosecond clock.
type Timeline struct {
	MovieTimescale uint32
	Tracks         []*Track
	Clips          []ClipPoint
}

const nanosPerSecond uint64 = 1_000_000_000

// TicksToNS converts a duration in an arbitrary timescale to nanoseconds.
// It reports an error on overflow instead of silently wrapping.
func TicksToNS(ticks uint64, timescale uint32) (int64, error) {
	if timescale == 0 {
		return 0, errors.New("zero timescale")
	}
	if ticks > (1<<63-1)/nanosPerSecond {
		return 0, errors.New("nanosecond conversion overflow")
	}
	v := ticks * nanosPerSecond
	if v/nanosPerSecond != ticks {
		return 0, errors.New("nanosecond multiplication overflow")
	}
	if v/uint64(timescale) > (1<<63 - 1) {
		return 0, errors.New("nanosecond conversion overflow")
	}
	return int64(v / uint64(timescale)), nil
}

// Median returns the median of a sorted copy of vals (ok=false when empty).
func Median(vals []int64) (int64, bool) {
	n := len(vals)
	if n == 0 {
		return 0, false
	}
	cp := append([]int64(nil), vals...)
	for i := 1; i < n; i++ {
		for j := i; j > 0 && cp[j-1] > cp[j]; j-- {
			cp[j-1], cp[j] = cp[j], cp[j-1]
		}
	}
	if n%2 == 1 {
		return cp[n/2], true
	}
	return (cp[n/2-1] + cp[n/2]) / 2, true
}
