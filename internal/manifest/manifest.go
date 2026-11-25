// Package manifest discovers media segments from local HLS playlists and
// DASH MPDs. Parsers never touch the network: URIs are resolved and validated
// against a local root, and every diagnostic carries a source position.
package manifest

import (
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"mediad/internal/media"
)

// Rendition describes one selectable presentation: a camera feed, an alternate
// language, or a role variant. Mappers use it to build media.TrackKey values.
type Rendition struct {
	Camera    string `json:"camera"`
	Language  string `json:"language"`
	Codec     string `json:"codec"`
	Role      string `json:"role"`
	Bandwidth int    `json:"bandwidth,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	URI       string `json:"uri"`
	Type      string `json:"type,omitempty"` // "video", "audio"
}

// Segment is one media object to fetch from the local root. Seq is the
// canonical sequence number (HLS media sequence + index, or DASH index).
type Segment struct {
	Seq           int             `json:"seq"`
	URI           string          `json:"uri"`
	RelPath       string          `json:"rel_path"`
	Duration      media.Rational  `json:"duration"`
	StartTicks    int64           `json:"start_ticks,omitempty"`
	Discontinuity bool            `json:"discontinuity,omitempty"`
	Rendition     *Rendition      `json:"rendition,omitempty"`
	Pos           media.SourcePos `json:"pos"`
}

// Manifest is the normalized result of parsing either playlist format.
type Manifest struct {
	Format     string       `json:"format"` // "hls" or "dash"
	File       string       `json:"file"`
	Renditions []*Rendition `json:"renditions,omitempty"`
	Segments   []Segment    `json:"segments"`
	Timescale  int64        `json:"timescale"`
}

// Options configures parsing and local URI resolution.
type Options struct {
	// Root is the directory that all segment and sub-playlist URIs must stay
	// inside. It is made absolute before use.
	Root  string
	Limit media.Limits
}

func (o *Options) limits() media.Limits {
	if o.Limit == (media.Limits{}) {
		return media.DefaultLimits()
	}
	return o.Limit
}

// ResolveURI validates uri from a playlist located at base and returns both
// the local absolute path and a clean path relative to root. It rejects
// absolute URLs, scheme-relative URLs, and any path escaping root. pos is
// attached to every error so failures point at the offending line.
func ResolveURI(root, base, uri string, pos media.SourcePos) (abs, rel string, err error) {
	if uri == "" {
		return "", "", media.New(media.CodeManifest, "resolve-uri", pos, "empty URI")
	}
	if strings.ContainsRune(uri, '\x00') {
		return "", "", media.New(media.CodePathTraversal, "resolve-uri", pos, "NUL in URI")
	}
	u, perr := url.Parse(uri)
	if perr != nil {
		return "", "", media.Wrap(media.CodeManifest, "parse-uri", pos, perr, "URI %q", uri)
	}
	if u.Scheme != "" || u.Host != "" {
		return "", "", media.New(media.CodeURIOutOfRange, "resolve-uri", pos,
			"non-local URI %q rejected", uri)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", media.Wrap(media.CodeIO, "root", media.SourcePos{}, err, "")
	}
	rootAbs = filepath.Clean(rootAbs)

	// Playlist-relative resolution, then containment check.
	var joined string
	if strings.HasPrefix(uri, "/") {
		// Root-relative path inside the manifest root.
		joined = filepath.Join(rootAbs, filepath.FromSlash(strings.TrimPrefix(uri, "/")))
	} else {
		baseDir := filepath.Dir(base)
		joined = filepath.Join(baseDir, filepath.FromSlash(u.Path))
	}
	joined = filepath.Clean(joined)

	relOS, err := filepath.Rel(rootAbs, joined)
	if err != nil {
		return "", "", media.New(media.CodePathTraversal, "resolve-uri", pos,
			"cannot relativize %q", joined)
	}
	if relOS == ".." || strings.HasPrefix(relOS, ".."+string(filepath.Separator)) {
		return "", "", media.New(media.CodePathTraversal, "resolve-uri", pos,
			"URI %q escapes root", uri)
	}
	// Query strings and fragments are meaningless for local media.
	rel = path.Clean(filepath.ToSlash(relOS))
	if rel == "." {
		rel = path.Base(filepath.ToSlash(joined))
	}
	return joined, rel, nil
}

// checkCount enforces the segment cap while appending.
func checkSegLimit(l media.Limits, n int, pos media.SourcePos) error {
	if l.MaxSegments > 0 && n > l.MaxSegments {
		return media.New(media.CodeLimit, "segments", pos, "segment count %d exceeds limit %d", n, l.MaxSegments)
	}
	return nil
}

// ValidateSegments rejects duplicate sequence numbers and turns a stable,
// sorted list of segments. Duplicates are a hard failure, not silently merged.
func ValidateSegments(segs []Segment) error {
	seen := make(map[int]media.SourcePos, len(segs))
	for _, s := range segs {
		if prev, ok := seen[s.Seq]; ok {
			return media.New(media.CodeDuplicateSeq, "validate-segments", s.Pos,
				"sequence %d duplicated (first at %s)", s.Seq, prev.String())
		}
		seen[s.Seq] = s.Pos
	}
	return nil
}

// Summary returns a compact human-readable description.
func (m *Manifest) Summary() string {
	return fmt.Sprintf("%s %s segments=%d renditions=%d timescale=%d",
		m.Format, filepath.Base(m.File), len(m.Segments), len(m.Renditions), m.Timescale)
}
