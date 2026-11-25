// Package manifest parses local HLS and DASH playlists, produces track
// descriptions and time anchors, and enforces the security boundary that
// only same-tree local references may be opened.
package manifest

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Kind identifies the streaming manifest format.
type Kind string

const (
	KindHLS  Kind = "hls"
	KindDASH Kind = "dash"
)

// TrackKind is the broad media role of a track.
type TrackKind string

const (
	TrackVideo TrackKind = "video"
	TrackAudio TrackKind = "audio"
	TrackData  TrackKind = "data"
)

// TrackDesc carries the track-level metadata that must be preserved
// verbatim across timestamp repair.
type TrackDesc struct {
	ID               string    `json:"id"`
	Kind             TrackKind `json:"kind"`
	Codec            string    `json:"codec,omitempty"`
	Timescale        uint32    `json:"timescale"`
	Channels         int       `json:"channels,omitempty"`
	ChannelLayout    string    `json:"channel_layout,omitempty"`
	SampleRate       uint32    `json:"sample_rate,omitempty"`
	ColorInfo        string    `json:"color_info,omitempty"`
	MetadataConflict string    `json:"metadata_conflict,omitempty"`
}

// Segment is one playlist entry in presentation order.
type Segment struct {
	TrackID       string  `json:"track_id,omitempty"`
	Index         int     `json:"index"`
	Sequence      int64   `json:"sequence"`
	URI           string  `json:"uri"`
	MapURI        string  `json:"map_uri,omitempty"`
	DurationSec   float64 `json:"duration_sec,omitempty"`
	DurationTicks int64   `json:"duration_ticks,omitempty"`
	StartTicks    int64   `json:"start_ticks,omitempty"`
	Discontinuity bool    `json:"discontinuity,omitempty"`
}

// AnchorKind enumerates why a timeline anchor exists.
type AnchorKind string

const (
	AnchorDiscontinuity AnchorKind = "discontinuity"
	AnchorMap           AnchorKind = "map"
	AnchorSequence      AnchorKind = "media_sequence"
	AnchorPTO           AnchorKind = "presentation_time_offset"
	AnchorTimeline      AnchorKind = "timeline"
)

// Anchor is a manifest-declared time anchor for one track.
type Anchor struct {
	Kind         AnchorKind `json:"kind"`
	TrackID      string     `json:"track_id,omitempty"`
	SegmentIndex int        `json:"segment_index"`
	Ticks        int64      `json:"ticks,omitempty"`
	Detail       string     `json:"detail,omitempty"`
}

// Manifest is the format-neutral playlist model consumed by repair.
type Manifest struct {
	Kind                   Kind        `json:"kind"`
	SourcePath             string      `json:"source_path"`
	Tracks                 []TrackDesc `json:"tracks"`
	Segments               []Segment   `json:"segments"`
	Anchors                []Anchor    `json:"anchors,omitempty"`
	MediaSequence          int64       `json:"media_sequence,omitempty"`
	PresentationTimeOffset int64       `json:"presentation_time_offset,omitempty"`
}

// Track returns the described track by id.
func (m *Manifest) Track(id string) (TrackDesc, bool) {
	for _, t := range m.Tracks {
		if t.ID == id {
			return t, true
		}
	}
	return TrackDesc{}, false
}

// IsRemote reports whether ref names a remote URI. Any URI with a
// scheme (http, https, file, ...) is rejected: the tool only processes
// locally provided assets.
func IsRemote(ref string) bool {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "//") {
		return true
	}
	u, err := url.Parse(ref)
	if err != nil {
		return false
	}
	return u.Scheme != ""
}

// CheckRef rejects remote URIs, absolute paths and any relative path
// that escapes the manifest directory via "..".
func CheckRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("manifest: empty URI reference")
	}
	if IsRemote(ref) {
		return fmt.Errorf("manifest: remote URI rejected: %s", ref)
	}
	if filepath.IsAbs(ref) || strings.HasPrefix(ref, "/") {
		return fmt.Errorf("manifest: absolute path rejected: %s", ref)
	}
	clean := filepath.ToSlash(filepath.Clean(ref))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("manifest: path traversal rejected: %s", ref)
	}
	return nil
}

// ResolveLocal joins baseDir with ref after running CheckRef and
// verifies the result stays inside baseDir.
func ResolveLocal(baseDir, ref string) (string, error) {
	if err := CheckRef(ref); err != nil {
		return "", err
	}
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(absBase, filepath.FromSlash(ref))
	rel, err := filepath.Rel(absBase, joined)
	if err != nil {
		return "", fmt.Errorf("manifest: cannot relativize %s: %w", ref, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("manifest: path escapes root: %s", ref)
	}
	return joined, nil
}
