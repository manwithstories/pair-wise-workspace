// Package manifest parses HLS playlists and DASH MPDs into a common segment
// description. Every reference is checked against the input root and carries
// the manifest line and byte offset it came from so downstream faults can point
// at the source.
package manifest

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"aircheck/internal/media"
)

// Artifact is an in-memory manifest with its logical name (a root-relative
// slash path, never an absolute path).
type Artifact struct {
	Name string
	Data []byte
}

// Line is one physical line of a text manifest.
type Line struct {
	Num    int    // 1-based
	Offset int64  // byte offset of the start of the line
	Text   string // without trailing CR/LF
}

// ScanLines splits data into lines, accepting LF and CRLF, stripping a UTF-8
// BOM from the first line. Byte offsets index the original data.
func ScanLines(data []byte) []Line {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		data = data[3:]
	}
	var out []Line
	off, num := int64(0), 1
	for len(data) > 0 {
		i := 0
		for i < len(data) && data[i] != '\n' {
			i++
		}
		raw := data[:i]
		rawLen := i
		if i < len(data) {
			i++
		}
		if rawLen > 0 && raw[rawLen-1] == '\r' {
			raw = raw[:rawLen-1]
		}
		out = append(out, Line{Num: num, Offset: off, Text: string(raw)})
		off += int64(i)
		num++
		data = data[i:]
	}
	return out
}

// LineAt returns the 1-based line number containing byte offset off.
func LineAt(data []byte, off int64) int {
	if off <= 0 {
		return 1
	}
	if off > int64(len(data)) {
		off = int64(len(data))
	}
	return 1 + strings.Count(string(data[:off]), "\n")
}

// ValidateURI resolves raw against the input root. It rejects absolute URLs
// (no network material), absolute filesystem paths, scheme tricks, control
// characters and any cleaned path that escapes the root. On success it returns
// the cleaned root-relative slash path.
func ValidateURI(root, raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: empty URI", media.FaultManifestRange)
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: URI contains control character 0x%02x", media.FaultManifestRange, r)
		}
	}
	if strings.Contains(raw, "\\") {
		return "", fmt.Errorf("%w: URI %q uses backslash separators", media.FaultManifestRange, raw)
	}
	if strings.Contains(raw, "://") || strings.HasPrefix(raw, "//") {
		return "", fmt.Errorf("%w: URI %q is not local", media.FaultManifestRange, raw)
	}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" {
		return "", fmt.Errorf("%w: URI %q has scheme %q", media.FaultManifestRange, raw, u.Scheme)
	}
	if strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("%w: URI %q is absolute", media.FaultManifestRange, raw)
	}
	if strings.Contains(strings.ToLower(raw), "%2e") || strings.Contains(raw, "%2f") || strings.Contains(raw, "%5c") {
		return "", fmt.Errorf("%w: URI %q uses percent-encoded dot segments", media.FaultManifestRange, raw)
	}
	// Query strings are not supported by the local fixture store; a
	// fragment is never a valid media file. Reject both rather than strip,
	// so a reference cannot hide a second path behind an ignored suffix.
	if strings.ContainsAny(raw, "?#") {
		return "", fmt.Errorf("%w: URI %q has a query or fragment", media.FaultManifestRange, raw)
	}
	clean := path.Clean(raw)
	if clean == "." || clean == "" {
		return "", fmt.Errorf("%w: URI %q resolves to the root", media.FaultManifestRange, raw)
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: URI %q escapes input root", media.FaultManifestRange, raw)
	}
	if root != "" && strings.HasPrefix(path.Clean(root), clean) {
		return "", fmt.Errorf("%w: URI %q aliases a parent directory", media.FaultManifestRange, raw)
	}
	return clean, nil
}

// SegRef is one media segment or init section reference.
type SegRef struct {
	URI       string // cleaned root-relative path
	Raw       string
	Line      int
	Offset    int64 // byte offset of the URI line inside the manifest
	RangeFrom int64
	RangeLen  int64
}

// Segment is a segment with its declared timing.
type Segment struct {
	Seq           int64
	Ref           SegRef
	Duration      media.Duration
	DurationSec   float64
	TotalTicks    int64 // absolute start tick on the segment timescale
	Discontinuity bool
	Init          *SegRef
}

// Media is a parsed media playlist. Ticks are in the caller-supplied clock so
// the declared EXTINF durations stay exact rational values.
type Media struct {
	Artifact         string
	Version          uint8
	TargetDuration   media.Duration
	TargetSec        float64
	SeqStart         int64
	EndList          bool
	Segments         []Segment
	Init             *SegRef
	DiscontinuitySeq int64
}

// TotalDuration sums the declared segment durations.
func (m *Media) TotalDuration() media.Duration {
	if len(m.Segments) == 0 {
		return media.Duration{}
	}
	return m.Segments[len(m.Segments)-1].Duration
}

// EndTick returns the declared end instant for the last segment.
func (m *Media) EndTick() int64 {
	if len(m.Segments) == 0 {
		return 0
	}
	last := m.Segments[len(m.Segments)-1]
	return last.TotalTicks + last.Duration.Ticks()
}

// duplicateSeq finds a repeated sequence number or repeated payload identity.
func duplicateSeq(seenSeq map[int64]int, seenID map[string]int, seq int64, id string, line int) error {
	if prev, ok := seenSeq[seq]; ok {
		return fmt.Errorf("duplicate sequence %d (first seen line %d)", seq, prev)
	}
	if prev, ok := seenID[id]; ok {
		return fmt.Errorf("duplicate segment payload %q (first seen line %d)", id, prev)
	}
	seenSeq[seq] = line
	seenID[id] = line
	return nil
}
