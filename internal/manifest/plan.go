// Package manifest parses HLS playlists and DASH manifests into a unified
// segment plan. The plan is the single source of ordering, duration and URI
// information used by the repair, probe and index stages.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// Format identifies the manifest dialect a plan came from.
type Format string

const (
	FormatHLS  Format = "hls"
	FormatDASH Format = "dash"
)

// Sentinel errors. Callers may test with errors.Is.
var (
	ErrTooLarge      = errors.New("manifest exceeds size or count limit")
	ErrPathTraversal = errors.New("segment URI escapes the manifest directory")
	ErrDuplicateURI  = errors.New("duplicate segment URI")
	ErrInvalid       = errors.New("invalid manifest")
)

// Options bounds the parser. Zero values select the defaults below.
type Options struct {
	MaxBytes           int
	MaxSegments        int
	MaxRepresentations int
}

// Defaults for Options.
const (
	DefaultMaxBytes           = 2 << 20
	DefaultMaxSegments        = 100_000
	DefaultMaxRepresentations = 64
)

func (o Options) normalize() Options {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxSegments <= 0 {
		o.MaxSegments = DefaultMaxSegments
	}
	if o.MaxRepresentations <= 0 {
		o.MaxRepresentations = DefaultMaxRepresentations
	}
	return o
}

// ByteRange is an optional HLS sub-range in bytes.
type ByteRange struct {
	Offset int64
	Length int64
}

// Segment is one entry of the unified plan.
type Segment struct {
	URI           string
	Sequence      int64
	StartNs       int64
	DurationNs    int64
	Discontinuity bool
	ByteRange     *ByteRange
	InitURI       string
}

// Plan is the segment plan for a single representation (HLS rendition or DASH
// Representation).
type Plan struct {
	Format          Format
	Representation  string
	Bandwidth       int64
	MimeType        string
	Codecs          string
	Timescale       uint32
	Segments        []Segment
	DurationNs      int64
	Discontinuities int
	Warnings        []string
	Digest          string
}

// Group is the set of representations parsed from one manifest document.
type Group struct {
	Format     Format
	Plans      []*Plan
	DurationNs int64
	Warnings   []string
	Digest     string
}

// Validate enforces URI safety and bound violations on a single plan.
func (p *Plan) Validate(opts Options) error {
	opts = opts.normalize()
	if len(p.Segments) > opts.MaxSegments {
		return fmt.Errorf("%w: %d segments", ErrTooLarge, len(p.Segments))
	}
	seen := make(map[string]struct{}, len(p.Segments))
	for i := range p.Segments {
		s := &p.Segments[i]
		if err := ValidateURI(s.URI); err != nil {
			return fmt.Errorf("segment %d: %w", i, err)
		}
		if s.InitURI != "" {
			if err := ValidateURI(s.InitURI); err != nil {
				return fmt.Errorf("init segment %d: %w", i, err)
			}
		}
		key := s.URI
		if s.ByteRange != nil {
			key = fmt.Sprintf("%s#%d-%d", s.URI, s.ByteRange.Offset, s.ByteRange.Length)
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: %s", ErrDuplicateURI, key)
		}
		seen[key] = struct{}{}
		if s.DurationNs < 0 {
			return fmt.Errorf("%w: negative duration for %s", ErrInvalid, s.URI)
		}
	}
	p.Digest = planDigest(p)
	return nil
}

// ValidateURI rejects absolute, scheme-bearing and traversing segment URIs.
// It deliberately allows relative paths with query strings.
func ValidateURI(raw string) error {
	if raw == "" {
		return fmt.Errorf("%w: empty URI", ErrInvalid)
	}
	if strings.ContainsAny(raw, "\r\n\x00") {
		return fmt.Errorf("%w: control character in URI", ErrInvalid)
	}
	if strings.Contains(raw, "://") || strings.HasPrefix(raw, "//") {
		return fmt.Errorf("%w: absolute URI %q", ErrInvalid, raw)
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return fmt.Errorf("%w: malformed escape in %q", ErrInvalid, raw)
	}
	slash := strings.ReplaceAll(decoded, "\\", "/")
	if strings.HasPrefix(slash, "/") {
		return fmt.Errorf("%w: absolute path %q", ErrInvalid, raw)
	}
	// path.Clean collapses traversal so it can be detected reliably.
	clean := path.Clean(slash)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%w: %q", ErrPathTraversal, raw)
	}
	for _, part := range strings.Split(slash, "/") {
		if part == ".." {
			return fmt.Errorf("%w: %q", ErrPathTraversal, raw)
		}
	}
	if strings.Contains(raw, "\x00") {
		return fmt.Errorf("%w: NUL in URI", ErrInvalid)
	}
	return nil
}

func planDigest(p *Plan) string {
	h := sha256.New()
	fmt.Fprintf(h, "format=%s\nrep=%s\nbandwidth=%d\nmime=%s\ncodecs=%s\ntimescale=%d\n",
		p.Format, p.Representation, p.Bandwidth, p.MimeType, p.Codecs, p.Timescale)
	for _, s := range p.Segments {
		fmt.Fprintf(h, "%d|%s|%d|%d|%t|", s.Sequence, s.URI, s.StartNs, s.DurationNs, s.Discontinuity)
		if s.ByteRange != nil {
			fmt.Fprintf(h, "%d+%d|", s.ByteRange.Offset, s.ByteRange.Length)
		}
		fmt.Fprintf(h, "%s\n", s.InitURI)
	}
	sum := sha256.Sum256(h.Sum(nil))
	return hex.EncodeToString(sum[:])
}

// finalizeGroup validates every plan, enforces consistent durations across
// representations and computes the group digest.
func finalizeGroup(g *Group, opts Options) error {
	opts = opts.normalize()
	if len(g.Plans) == 0 {
		return fmt.Errorf("%w: no representations", ErrInvalid)
	}
	if len(g.Plans) > opts.MaxRepresentations {
		return fmt.Errorf("%w: %d representations", ErrTooLarge, len(g.Plans))
	}
	var base int64
	var baseName string
	for _, p := range g.Plans {
		if err := p.Validate(opts); err != nil {
			return fmt.Errorf("representation %s: %w", p.Representation, err)
		}
		if baseName == "" {
			base, baseName = p.DurationNs, p.Representation
			continue
		}
		delta := p.DurationNs - base
		if delta < 0 {
			delta = -delta
		}
		if delta > 1_000_000 {
			return fmt.Errorf("%w: representation %s lasts %dns, %s lasts %dns",
				ErrInvalid, p.Representation, p.DurationNs, baseName, base)
		}
	}
	g.DurationNs = base
	h := sha256.New()
	for _, p := range g.Plans {
		fmt.Fprintf(h, "%s\n", p.Digest)
	}
	sum := sha256.Sum256(h.Sum(nil))
	g.Digest = hex.EncodeToString(sum[:])
	return nil
}
