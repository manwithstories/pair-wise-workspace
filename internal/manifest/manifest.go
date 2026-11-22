// Package manifest parses HLS and DASH playlists into a unified
// track/segment model on the shared rational clock.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"

	"mediacore/internal/clock"
)

const (
	// MaxManifestBytes bounds a single playlist document.
	MaxManifestBytes = 8 << 20
	// MaxSegments bounds the number of segments per track.
	MaxSegments = 100000
)

// Segment is one media segment declared by a playlist.
type Segment struct {
	Index int
	URI   string
	Dur   clock.Tick
	Gap   bool // declared hole: no media behind this range
}

// TrackMeta carries the static properties of a track.
type TrackMeta struct {
	ID        string
	Kind      string // "video" or "audio"
	Codec     string // "avc1" or "mp4a"
	Timescale int64
}

// Track is a parsed track: metadata plus ordered segments.
type Track struct {
	TrackMeta
	Segments []Segment
}

// Playlist is the unified parse result of one manifest document.
type Playlist struct {
	Format string // "hls" or "dash"
	Tracks []Track
}

var supportedCodecs = map[string]string{
	"avc1": "video",
	"h264": "video",
	"mp4a": "audio",
	"aac":  "audio",
}

// ValidateCodec rejects unsupported codecs.
func ValidateCodec(codec string) error {
	if _, ok := supportedCodecs[codec]; !ok {
		return clock.At(clock.CodeCodecUnsupported, "validate", "", -1,
			"unsupported codec %q", codec)
	}
	return nil
}

// ValidateURI rejects path escapes and absolute paths.
func ValidateURI(u string) error {
	if u == "" {
		return clock.At(clock.CodeManifestInvalid, "validate", "", -1, "empty segment URI")
	}
	if strings.HasPrefix(u, "/") || strings.HasPrefix(u, "\\") ||
		strings.Contains(u, "..") || strings.ContainsAny(u, "\\\x00") ||
		strings.Contains(u, "://") {
		return clock.At(clock.CodePathEscape, "validate", "", -1,
			"unsafe segment URI %q", u)
	}
	return nil
}

// ValidateTrack checks codec, timescale and segment URIs.
func ValidateTrack(t Track) error {
	if err := ValidateCodec(t.Codec); err != nil {
		return err
	}
	if t.Timescale <= 0 {
		return clock.At(clock.CodeTimescaleInvalid, "validate", t.ID, -1,
			"timescale %d", t.Timescale)
	}
	if len(t.Segments) > MaxSegments {
		return clock.At(clock.CodeManifestTooLarge, "validate", t.ID, -1,
			"%d segments exceeds %d", len(t.Segments), MaxSegments)
	}
	for _, s := range t.Segments {
		if s.Gap {
			continue
		}
		if err := ValidateURI(s.URI); err != nil {
			e := err.(*clock.Error)
			e.Track, e.Segment = t.ID, s.Index
			return e
		}
	}
	return nil
}

// Digest returns a deterministic content digest of the playlist.
func (p *Playlist) Digest() string {
	h := sha256.New()
	io.WriteString(h, p.Format+"\n")
	tracks := make([]Track, len(p.Tracks))
	copy(tracks, p.Tracks)
	sort.Slice(tracks, func(i, j int) bool { return tracks[i].ID < tracks[j].ID })
	for _, t := range tracks {
		fmt.Fprintf(h, "%s|%s|%s|%d\n", t.ID, t.Kind, t.Codec, t.Timescale)
		for _, s := range t.Segments {
			fmt.Fprintf(h, "%d|%s|%s|%v\n", s.Index, s.URI, s.Dur, s.Gap)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ParseDecimal converts a non-negative decimal string ("2", "2.005")
// into an exact rational tick.
func ParseDecimal(s string) (clock.Tick, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return clock.Tick{}, fmt.Errorf("empty number")
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	parts := strings.SplitN(s, ".", 2)
	num := parts[0]
	den := "1"
	if len(parts) == 2 {
		den = "1" + strings.Repeat("0", len(parts[1]))
		num += parts[1]
	}
	var n, d int64
	if _, err := fmt.Sscanf(num, "%d", &n); err != nil {
		return clock.Tick{}, fmt.Errorf("bad number %q", s)
	}
	if _, err := fmt.Sscanf(den, "%d", &d); err != nil {
		return clock.Tick{}, fmt.Errorf("bad number %q", s)
	}
	if neg {
		n = -n
	}
	return clock.New(n, d)
}
