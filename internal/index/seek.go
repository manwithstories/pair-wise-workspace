// Package index builds a random-access seek index restricted to valid
// sync samples and commits it atomically after verification.
package index

import (
	"avrepair/internal/media"
)

// Version is the index schema version.
const Version = 1

// Entry is one seekable point. It never references a quarantined or
// missing segment.
type Entry struct {
	Track      string          `json:"track"`
	Kind       media.TrackType `json:"kind"`
	Segment    string          `json:"segment"`
	Source     string          `json:"source"`
	SourceURI  string          `json:"source_uri"`
	SourceSHA  string          `json:"source_sha256"`
	Output     string          `json:"output"`
	PTS        media.Rational  `json:"pts"`
	Duration   media.Rational  `json:"duration"`
	SegmentSeq int             `json:"segment_seq"`
	Sync       bool            `json:"sync"`
	TimeMS     int64           `json:"time_ms"`
}

// Index is the committed random-access map.
type Index struct {
	Version    int     `json:"version"`
	InputHash  string  `json:"input_sha256"`
	ResultHash string  `json:"result_sha256,omitempty"`
	Tracks     []Track `json:"tracks"`
	Entries    []Entry `json:"entries"`
}

// Track summarizes one indexed track.
type Track struct {
	ID         string          `json:"id"`
	Kind       media.TrackType `json:"kind"`
	Entries    int             `json:"entries"`
	Start      media.Rational  `json:"start"`
	End        media.Rational  `json:"end"`
	Quarantine int             `json:"quarantined"`
}
