// Package manifest parses HLS media playlists and DASH SegmentTimeline
// documents into a single SegmentEvent stream. Parsing is pure: it never
// opens network connections and rejects path traversal, conflicting
// duplicate sequence numbers and oversize playlists.
package manifest

import (
	"fmt"
	"strings"

	"github.com/clearliu/fieldcut/internal/clock"
)

// Protocol identifies the manifest flavor that produced an event.
type Protocol int

const (
	ProtocolUnknown Protocol = iota
	ProtocolHLS
	ProtocolDASH
)

func (p Protocol) String() string {
	switch p {
	case ProtocolHLS:
		return "hls"
	case ProtocolDASH:
		return "dash"
	default:
		return "unknown"
	}
}

// Kind classifies a playlist line / manifest element.
type Kind int

const (
	KindMedia Kind = iota
	KindInit // initialization segment (fMP4 moov)
)

// SegmentEvent is the normalized output of both parsers. Sequence is the
// monotonically increasing index used for duplicate detection; URI is the
// already-validated relative path of the segment file.
type SegmentEvent struct {
	Protocol Protocol
	Kind     Kind
	TrackID  int
	Sequence int
	URI      string
	// Start/Duration live on the 90 kHz axis; DASH supplies them from
	// SegmentTimeline, HLS from EXTINF.
	Start    clock.Ticks
	Duration clock.Ticks
	// Discontinuity marks an HLS EXT-X-DISCONTINUITY boundary.
	Discontinuity bool
	// ByteRange is non-nil when the segment is addressed by bytes.
	ByteRange *ByteRange
}

// ByteRange is a validated [start, start+length) byte span.
type ByteRange struct {
	Start  int64
	Length int64
}

// End returns the exclusive end offset.
func (b ByteRange) End() int64 { return b.Start + b.Length }

// Options carries hard limits for both parsers. Zero fields fall back to
// the shared caps from the clock package.
type Options struct {
	MaxSegments int
	MaxTracks   int
	// MaxURIBytes rejects absurdly long segment paths.
	MaxURIBytes int
}

func (o *Options) withDefaults() Options {
	c := *o
	if c.MaxSegments <= 0 {
		c.MaxSegments = clock.MaxManifestSegments
	}
	if c.MaxTracks <= 0 {
		c.MaxTracks = clock.MaxTracks
	}
	if c.MaxURIBytes <= 0 {
		c.MaxURIBytes = 4096
	}
	return c
}

// CheckSequenceConflicts validates a merged event set (possibly produced
// from several HLS playlists and/or DASH representations): on any given
// track a sequence number must map to exactly one URI. Re-delivery of the
// same URI at the same sequence number is idempotent and accepted, which
// matches retries on flaky capture nodes. Init segments do not carry
// sequence numbers and are skipped.
func CheckSequenceConflicts(events []SegmentEvent) error {
	type key struct {
		track int
		seq   int
	}
	uris := map[key]string{}
	for _, e := range events {
		if e.Kind == KindInit {
			continue
		}
		k := key{e.TrackID, e.Sequence}
		if prev, ok := uris[k]; ok && prev != e.URI {
			return fmt.Errorf("%w: track %d seq %d: %q vs %q",
				clock.ErrDuplicateSeq, e.TrackID, e.Sequence, prev, e.URI)
		}
		uris[k] = e.URI
	}
	return nil
}

// safePath rejects absolute paths, volume names, any ".." traversal and
// backslash separators, returning the cleaned relative path. It is the
// single chokepoint both parsers use before emitting a URI.
func safePath(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: empty uri", clock.ErrInvalidArgument)
	}
	if len(raw) > 4096 {
		return "", fmt.Errorf("%w: uri longer than 4096 bytes", clock.ErrInvalidArgument)
	}
	if strings.ContainsRune(raw, '\\') || strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("%w: %q", clock.ErrPathTraversal, raw)
	}
	if strings.Contains(raw, "://") {
		return "", fmt.Errorf("%w: remote scheme not allowed: %q", clock.ErrPathTraversal, raw)
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: %q", clock.ErrPathTraversal, raw)
		}
	}
	clean := strings.TrimPrefix(strings.TrimSpace(raw), "./")
	if clean == "" || clean == "." {
		return "", fmt.Errorf("%w: empty uri after cleaning", clock.ErrInvalidArgument)
	}
	return clean, nil
}
