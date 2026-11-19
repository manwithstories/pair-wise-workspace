// Package demux reads MP4/MOV containers into a unified in-memory model:
// tracks, per-sample timestamps on the canonical timeline, keyframe flags
// and availability information. It tolerates truncated tails, damaged
// sample tables and missing indexes, recording every anomaly as a fault
// with file offset and media time.
package demux

import "mediapack/internal/fault"

// Sample describes one media sample. DTS/PTS/Duration are expressed in the
// track's own timescale; conversion to the canonical timeline happens via
// the clock package.
type Sample struct {
	Index     uint32
	DTS       int64
	PTS       int64
	Duration  uint32
	Size      uint32
	Offset    int64
	Keyframe  bool
	Available bool // payload bytes are actually present in the file
}

// Track is one demuxed track.
type Track struct {
	ID            uint32
	Kind          string // "video", "audio" or "other"
	Codec         string // sample entry type, e.g. "avc1", "mp4a"
	Timescale     uint32
	Samples       []Sample
	IndexComplete bool // random-access index (stss) was present
}

// Duration returns the track duration in track timescale units.
func (t *Track) Duration() int64 {
	var d int64
	for _, s := range t.Samples {
		if end := s.DTS + int64(s.Duration); end > d {
			d = end
		}
	}
	return d
}

// Interval is a half-open range on the canonical timeline.
type Interval struct {
	StartTicks int64
	EndTicks   int64
	Reason     string
}

// Container is the demuxed representation of one input file.
type Container struct {
	Path           string
	Size           int64
	Fingerprint    string // hex SHA-256 of the whole file
	MovieTimescale uint32
	DurationTicks  int64 // canonical ticks
	Tracks         []*Track
	Faults         *fault.List
	// Missing lists canonical-time intervals whose sample data is absent
	// from the file (truncation, damaged tables).
	Missing []Interval
	// Untrusted lists intervals whose timing could not be confirmed from
	// a complete index (e.g. keyframes recovered by payload scanning).
	Untrusted []Interval
}

// VideoTracks returns the video tracks sorted by ascending average bitrate,
// which is the representation order used by the ladder aligner.
func (c *Container) VideoTracks() []*Track {
	var out []*Track
	for _, t := range c.Tracks {
		if t.Kind == "video" {
			out = append(out, t)
		}
	}
	return out
}

// AudioTracks returns the audio tracks.
func (c *Container) AudioTracks() []*Track {
	var out []*Track
	for _, t := range c.Tracks {
		if t.Kind == "audio" {
			out = append(out, t)
		}
	}
	return out
}
