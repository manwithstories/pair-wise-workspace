// Package demux turns source packages (BMFF files and RTP/H.264 capture
// streams) into a stream of coded samples annotated with their source
// position. It never decodes or transcodes media: sample payloads are the
// original coded bytes, copied once and passed through untouched.
package demux

import (
	"archivecheck/internal/media"
)

// Container identifies the on-disk wrapping of a source file.
type Container string

const (
	ContainerBMFF       Container = "bmff"        // MP4 / MOV, single moov
	ContainerFragmented Container = "fragmented" // fragmented MP4 (moof/mdat)
	ContainerRTP        Container = "rtp"        // H.264 RTP capture stream
)

// FormatVersion is the archive format revision. It is part of every cache key
// so a format change automatically invalidates stale cached fragments.
const FormatVersion = "ac-bmff-1"

// Codec identifiers used by the generated fixtures and the remuxer.
const (
	CodecAVC1  = "avc1" // H.264 video, supported
	CodecMP4A  = "mp4a" // AAC audio, supported
	CodecText  = "text" // 3GPP timed text / subtitles, supported
	CodecVC01  = "vc01" // unsupported placeholder codec, must be isolated
)

// Sample is one coded unit (a video NAL access unit, an audio frame or a
// timed-text sample) together with everything needed to locate it in the
// source and place it on the timeline.
type Sample struct {
	Track     int    // 1-based track number within the source
	Index     int    // 0-based sample index within the track
	Data      []byte // original coded bytes, never re-encoded
	Offset    int64  // absolute byte offset of the payload in the source
	Duration  int64  // duration in the track's time base
	DTS       int64  // decode timestamp in the track's time base
	Keyframe  bool   // sync sample (from stss, or rebuilt by the index module)
	Codec     string
	TimeBase  media.Rational
	Fragment  int // 0 for plain BMFF, >=1 for the owning movie fragment
}

// TrackMeta describes one track discovered while demuxing.
type TrackMeta struct {
	Track    int
	Codec    string
	Handler  string // vide / soun / text
	TimeBase media.Rational
	Samples  int
	Keyframes []int // 1-based sample numbers from stss; nil when absent
}

// SourceReport summarizes one demux run, including non-fatal faults.
type SourceReport struct {
	Segment    string
	Container  Container
	Tracks     []TrackMeta
	Samples    int
	Truncated  bool       // tail was cut; last complete sample retained
	Faults     []*media.Fault
	ScanBytes  int64      // bytes inspected while rebuilding indexes
	ScanLimited bool      // scan hit the byte cap before finishing
}
