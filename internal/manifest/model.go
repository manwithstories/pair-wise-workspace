// Package manifest parses DASH and HLS playlists and validates fragment boundaries.
package manifest

import (
	"mediad/internal/clock"
)

// Segment is one media fragment with its period and initialization segment.
type Segment struct {
	Index      int            `json:"index"`
	Period     int            `json:"period"`
	URI        string         `json:"uri"`
	InitURI    string         `json:"init_uri"`
	Start      clock.Rational `json:"start"`
	Duration   clock.Rational `json:"duration"`
	ByteOffset int64          `json:"byte_offset"`
	ByteLength int64          `json:"byte_length"`
	LineOffset int64          `json:"line_offset"`
}

// Representation is one DASH adaptation representation.
type Representation struct {
	ID        string    `json:"id"`
	Codecs    string    `json:"codecs"`
	MimeType  string    `json:"mime_type"`
	Width     int       `json:"width,omitempty"`
	Height    int       `json:"height,omitempty"`
	Bandwidth int       `json:"bandwidth"`
	Segments  []Segment `json:"segments"`
}

// Manifest is the parser-neutral view of either adaptive playlist format.
type Manifest struct {
	Kind            string           `json:"kind"`
	Path            string           `json:"path"`
	Version         int              `json:"version"`
	Representations []Representation `json:"representations,omitempty"`
	Segments        []Segment        `json:"segments"`
}

// CodecDescriptor identifies the container and codecs at a parsing boundary.
type CodecDescriptor struct {
	Path           string `json:"path,omitempty"`
	Container      string `json:"container"`
	VideoCodec     string `json:"video_codec,omitempty"`
	AudioCodec     string `json:"audio_codec,omitempty"`
	AdapterVersion int    `json:"adapter_version"`
	SpecVersion    string `json:"spec_version"`
}
