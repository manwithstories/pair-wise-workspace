// Package manifest parses HLS media playlists and DASH MPDs into one
// normalized, rational-clock timeline.
package manifest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mediascan/internal/model"
)

// Limits bounding manifest size and path traversal.
const (
	MaxManifestBytes = 4 << 20 // 4 MiB of text is far beyond any real playlist.
	MaxSegments      = 10000
)

// Errors raised by manifest parsing before any media is touched.
var (
	ErrUnsafePath   = errors.New("manifest: unsafe path")
	ErrUnsupported  = errors.New("manifest: unsupported construct")
	ErrBadManifest  = errors.New("manifest: malformed manifest")
	ErrTooLarge     = errors.New("manifest: manifest too large")
	ErrTooManyItems = errors.New("manifest: too many entries")
)

// Segment is one normalized manifest entry. Start/Duration are exact clock
// values, ByteStart/ByteEnd an optional byte range, and SHA256 the digest of
// the referenced media once it has been scanned.
type Segment struct {
	Name      string         `json:"name"`
	URI       string         `json:"uri"`
	ByteStart int64          `json:"byte_start"`
	ByteEnd   int64          `json:"byte_end"`
	Start     model.Rational `json:"start"`
	Duration  model.Rational `json:"duration"`
	SHA256    string         `json:"sha256,omitempty"`
	Present   bool           `json:"present"`
}

// End returns Start+Duration.
func (s Segment) End() (model.Rational, error) { return s.Start.Add(s.Duration) }

// Stream describes one media variant/representation.
type Stream struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"` // "video" | "audio" | "unknown"
	Timescale  uint32         `json:"timescale"`
	InitURI    string         `json:"init_uri,omitempty"`
	SegmentEnd model.Rational `json:"segment_end"`
	Segments   []Segment      `json:"segments"`
}

// Timeline is the normalized output of either manifest dialect.
type Timeline struct {
	Source     string         `json:"source"`
	Format     string         `json:"format"` // "hls" | "dash"
	Root       string         `json:"root"`
	Timescale  uint32         `json:"timescale"`
	Duration   model.Rational `json:"duration"`
	Streams    []Stream       `json:"streams"`
	Missing    []string       `json:"missing,omitempty"`
	FirstMiss  string         `json:"first_missing,omitempty"`
	Referenced []string       `json:"referenced,omitempty"`
}

// Resolver confines every relative reference to one workspace directory.
type Resolver struct {
	Root string
}

// NewResolver cleans and stores the workspace root.
func NewResolver(root string) (*Resolver, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsafePath, err)
	}
	return &Resolver{Root: filepath.Clean(abs)}, nil
}

// Resolve turns a manifest-relative URI into an absolute path inside Root.
// Absolute URIs, "..", NUL bytes and symlink escapes are all rejected.
func (r *Resolver) Resolve(uri string) (string, error) {
	if uri == "" {
		return "", fmt.Errorf("%w: empty reference", ErrUnsafePath)
	}
	if strings.ContainsRune(uri, 0) {
		return "", fmt.Errorf("%w: NUL byte in %q", ErrUnsafePath, uri)
	}
	if strings.Contains(uri, "://") || strings.HasPrefix(uri, "/") ||
		strings.HasPrefix(uri, `\`) || strings.Contains(uri, ":") {
		return "", fmt.Errorf("%w: absolute reference %q", ErrUnsafePath, uri)
	}
	cleaned := filepath.Clean(filepath.FromSlash(uri))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: traversal in %q", ErrUnsafePath, uri)
	}
	full := filepath.Join(r.Root, cleaned)
	rel, err := filepath.Rel(r.Root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: escapes root: %q", ErrUnsafePath, uri)
	}
	return full, nil
}

// Rel converts an absolute path back to a root-relative slash path.
func (r *Resolver) Rel(full string) (string, error) {
	rel, err := filepath.Rel(r.Root, full)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnsafePath, err)
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%w: outside root: %s", ErrUnsafePath, full)
	}
	return rel, nil
}

// ReadLimited reads at most MaxManifestBytes from path, refusing anything
// larger rather than silently truncating it.
func ReadLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	limited := io.LimitReader(f, MaxManifestBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxManifestBytes {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, path, MaxManifestBytes)
	}
	return data, nil
}

// wrapLine annotates a parse error with the manifest path and 1-based line.
func wrapLine(path string, line int, err error) error {
	return fmt.Errorf("%s:%d: %w", path, line+1, err)
}

// ValidateSegment applies the common rules every dialect must satisfy.
func (r *Resolver) ValidateSegment(seg Segment) (Segment, error) {
	if seg.Name == "" {
		return Segment{}, fmt.Errorf("%w: segment without name", ErrBadManifest)
	}
	if seg.Duration.Sign() < 0 {
		return Segment{}, fmt.Errorf("%w: %s has negative duration", ErrBadManifest, seg.Name)
	}
	if seg.Start.Sign() < 0 {
		return Segment{}, fmt.Errorf("%w: %s has negative start", ErrBadManifest, seg.Name)
	}
	if seg.ByteEnd < 0 && seg.ByteStart == 0 {
		// No byte range recorded.
	} else if seg.ByteEnd < seg.ByteStart || seg.ByteStart < 0 {
		return Segment{}, fmt.Errorf("%w: %s byte range %d-%d", ErrBadManifest, seg.Name, seg.ByteStart, seg.ByteEnd)
	}
	if seg.URI != "" {
		if _, err := r.Resolve(seg.URI); err != nil {
			return Segment{}, err
		}
	}
	return seg, nil
}

// statFile reports whether path exists as a regular file.
func statFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return info.Mode().IsRegular(), nil
}
