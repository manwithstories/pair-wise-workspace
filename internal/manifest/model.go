// Package manifest parses HLS media playlists and DASH MPD documents into a
// single segment model expressed in the canonical media time base.
//
// Native representation time bases, sequence numbers and original URI
// references are retained so that serialisation round trips preserve both
// fields and timeline.
package manifest

import "mediad/internal/clock"

// Kind identifies the manifest format.
type Kind string

const (
	KindHLS  Kind = "hls"
	KindDASH Kind = "dash"
)

// CanonicalTimescale is the time base used by every Segment field.
const CanonicalTimescale = clock.DefaultTimescale

// Range is an HLS byte range. HasOffset distinguishes "@0" from omission.
type Range struct {
	Length    int64 `json:"length"`
	Offset    int64 `json:"offset,omitempty"`
	HasOffset bool  `json:"has_offset,omitempty"`
}

// Key is a retained EXT-X-KEY tag.
type Key struct {
	Method            string `json:"method"`
	URI               string `json:"uri,omitempty"`
	IV                string `json:"iv,omitempty"`
	KeyFormat         string `json:"key_format,omitempty"`
	KeyFormatVersions string `json:"key_format_versions,omitempty"`
}

// Segment is the unified record consumed by probes, indexes, audio and
// pipelines.
type Segment struct {
	URI              string `json:"uri"`
	InitURI          string `json:"init_uri,omitempty"`
	RepresentationID string `json:"representation_id,omitempty"`
	Sequence         uint64 `json:"sequence"`
	Index            int    `json:"index"`
	Start            int64  `json:"start"`    // canonical ticks
	Duration         int64  `json:"duration"` // canonical ticks, 0 = unknown
	NativeTimescale  uint32 `json:"native_timescale"`
	NativeStart      int64  `json:"native_start"`
	NativeDuration   int64  `json:"native_duration"`
	DurationKnown    bool   `json:"duration_known"`
	Discontinuity    bool   `json:"discontinuity,omitempty"`
	ByteRange        *Range `json:"byte_range,omitempty"`
	MapURI           string `json:"map_uri,omitempty"`
	MapByteRange     *Range `json:"map_byte_range,omitempty"`
	Key              *Key   `json:"key,omitempty"`
}

// End returns the canonical end tick when the duration is known.
func (s Segment) End() int64 {
	if !s.DurationKnown {
		return s.Start
	}
	return s.Start + s.Duration
}

// TimelineEntry is one DASH S element.
type TimelineEntry struct {
	Time     int64 `json:"time"`
	HasTime  bool  `json:"has_time,omitempty"`
	Duration int64 `json:"duration"`
	Repeat   int64 `json:"repeat"`
}

// Template is a DASH SegmentTemplate after inheritance resolution.
type Template struct {
	Media                  string          `json:"media"`
	Initialization         string          `json:"initialization,omitempty"`
	Timescale              uint32          `json:"timescale"`
	Duration               int64           `json:"duration,omitempty"`
	HasDuration            bool            `json:"has_duration,omitempty"`
	StartNumber            uint64          `json:"start_number"`
	HasStartNumber         bool            `json:"has_start_number,omitempty"`
	PresentationTimeOffset int64           `json:"presentation_time_offset,omitempty"`
	HasPTO                 bool            `json:"has_pto,omitempty"`
	Timeline               []TimelineEntry `json:"timeline,omitempty"`
}

// Representation groups segments from one variant (HLS) or Representation
// (DASH).
type Representation struct {
	ID              string    `json:"id"`
	Bandwidth       int64     `json:"bandwidth,omitempty"`
	Codecs          string    `json:"codecs,omitempty"`
	MimeType        string    `json:"mime_type,omitempty"`
	Width           int       `json:"width,omitempty"`
	Height          int       `json:"height,omitempty"`
	InitURI         string    `json:"init_uri,omitempty"`
	NativeTimescale uint32    `json:"native_timescale"`
	Template        *Template `json:"template,omitempty"`
	Segments        []Segment `json:"segments"`
}

// DASHMeta retains MPD and Period attributes needed for an exact round trip.
type DASHMeta struct {
	Profiles                  string `json:"profiles,omitempty"`
	MinBufferTime             int64  `json:"min_buffer_time,omitempty"`
	HasMinBufferTime          bool   `json:"has_min_buffer_time,omitempty"`
	MediaPresentationDuration int64  `json:"media_presentation_duration,omitempty"`
	HasMPD                    bool   `json:"has_mpd_duration,omitempty"`
	PeriodID                  string `json:"period_id,omitempty"`
	PeriodStart               int64  `json:"period_start,omitempty"`
	HasPeriodStart            bool   `json:"has_period_start,omitempty"`
	PeriodDuration            int64  `json:"period_duration,omitempty"`
	HasPeriodDuration         bool   `json:"has_period_duration,omitempty"`
}

// Manifest is the parsed document. Faults never prevent access to segments
// that did parse.
type Manifest struct {
	Kind            Kind             `json:"kind"`
	Version         int              `json:"version,omitempty"`
	TargetDuration  int64            `json:"target_duration,omitempty"`
	MediaSequence   uint64           `json:"media_sequence,omitempty"`
	PlaylistType    string           `json:"playlist_type,omitempty"`
	EndList         bool             `json:"end_list,omitempty"`
	DASH            DASHMeta         `json:"dash,omitempty"`
	Representations []Representation `json:"representations"`
}

// Segments returns all segments in representation order.
func (m *Manifest) Segments() []Segment {
	var out []Segment
	for _, r := range m.Representations {
		out = append(out, r.Segments...)
	}
	return out
}

// SegmentCount returns the number of segments across representations.
func (m *Manifest) SegmentCount() int {
	n := 0
	for _, r := range m.Representations {
		n += len(r.Segments)
	}
	return n
}
