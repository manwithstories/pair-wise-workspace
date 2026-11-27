// Package manifest parses HLS (master + variant M3U8) and DASH (MPD)
// playlists into a single in-memory model. It enforces two delivery rules at
// parse time: referenced URIs must not escape the manifest directory (path
// traversal), and segment sequence numbers must be unique within a variant or
// representation (duplicate / out-of-order sequence).
package manifest

import (
	"errors"
	"path"
	"strings"
)

// Format identifies the adaptive streaming family.
type Format string

const (
	FormatHLS  Format = "hls"
	FormatDASH Format = "dash"
)

// Segment is a single media chunk reference.
type Segment struct {
	// Sequence is the manifest-level sequence number (unique per variant).
	Sequence int `json:"sequence"`
	// URI is the segment location relative to the manifest directory.
	URI string `json:"uri"`
	// Start90k / Duration90k place the segment on the shared 90 kHz clock.
	Start90k    int64 `json:"start_90k"`
	Duration90k int64 `json:"duration_90k"`
	// Disposition marks injected conditions such as a truncated packet.
	Disposition string `json:"disposition,omitempty"`
}

// Representation is one encoded rendition (DASH AdaptationSet/Representation
// or an HLS variant stream).
type Representation struct {
	// ID is unique within a camera and stable across formats.
	ID string `json:"id"`
	// Role is "primary" or "backup"; backup renditions drive recovery.
	Role string `json:"role"`
	// Codec, e.g. "h264+aac".
	Codec string `json:"codec"`
	// Bandwidth bits per second (declarative).
	Bandwidth int `json:"bandwidth"`
	// Width/Height for video renditions, zero for audio-only.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// SampleRate / Channels describe the audio track when present.
	SampleRate int `json:"sample_rate,omitempty"`
	Channels   int `json:"channels,omitempty"`
	// Segments in manifest order.
	Segments []Segment `json:"segments"`
}

// Variant is an HLS variant playlist entry (master playlist level).
type Variant struct {
	// URI points at the variant playlist, relative to the master playlist.
	URI string `json:"uri"`
	// Bandwidth / Resolution are master-playlist attributes.
	Bandwidth int    `json:"bandwidth"`
	Resolution string `json:"resolution,omitempty"`
	// Name maps the variant to a Representation role once expanded.
	Name string `json:"name"`
}

// Manifest is the parsed, format-agnostic view of one camera's playlist set.
type Manifest struct {
	// Format is hls or dash.
	Format Format `json:"format"`
	// Camera is the source-camera identifier.
	Camera string `json:"camera"`
	// BaseDir is the directory that segment URIs resolve against. It is always
	// cleaned and kept absolute by the parser.
	BaseDir string `json:"base_dir"`
	// MasterURI is the master playlist / MPD path relative to the camera root.
	MasterURI string `json:"master_uri"`
	// Variants holds HLS master-playlist entries (empty for DASH).
	Variants []Variant `json:"variants,omitempty"`
	// Representations holds the expanded renditions in encounter order.
	Representations []Representation `json:"representations"`
}

// ErrTraversal and friends are returned (possibly wrapped) by the parser.
var (
	ErrTraversal     = errors.New("manifest: URI escapes the manifest directory")
	ErrDuplicateSeq  = errors.New("manifest: duplicate segment sequence")
	ErrOutOfOrder    = errors.New("manifest: segment sequence out of order")
	ErrInvalidManifest = errors.New("manifest: invalid playlist")
)

// safeJoin resolves a URI against the manifest base directory and rejects any
// result that escapes baseDir. Only local relative paths are supported;
// absolute and scheme-qualified URIs are rejected.
func safeJoin(baseDir, uri string) (string, error) {
	if uri == "" {
		return "", ErrInvalidManifest
	}
	if strings.HasPrefix(uri, "/") || strings.Contains(uri, "://") {
		return "", ErrTraversal
	}
	clean := path.Clean(uri)
	if clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return "", ErrTraversal
		}
	}
	joined := path.Clean(path.Join(baseDir, clean))
	base := path.Clean(baseDir)
	if joined != base && !strings.HasPrefix(joined, base+"/") {
		return "", ErrTraversal
	}
	return joined, nil
}

// checkSequence enforces strictly increasing, unique sequence numbers and
// returns the first offending position together with the matching sentinel
// error.
func checkSequence(segs []Segment) (int, error) {
	seen := make(map[int]bool, len(segs))
	prev := -1
	for i, s := range segs {
		if seen[s.Sequence] {
			return i, ErrDuplicateSeq
		}
		seen[s.Sequence] = true
		if s.Sequence <= prev {
			return i, ErrOutOfOrder
		}
		prev = s.Sequence
	}
	return -1, nil
}
