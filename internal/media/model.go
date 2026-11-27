package media

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Hard limits applied to every untrusted input.
const (
	MaxSegmentBytes  int64 = 64 << 20 // 64 MiB per media segment
	MaxManifestBytes int64 = 2 << 20  // 2 MiB per manifest
	MaxTimeline            = 12 * time.Hour
)

// TrackType classifies a media track.
type TrackType string

const (
	TrackVideo TrackType = "video"
	TrackAudio TrackType = "audio"
	TrackOther TrackType = "other"
)

// Quarantine reasons. A reason is always recorded with the affected segment.
const (
	ReasonTruncated        = "truncated_segment"
	ReasonUnparseable      = "unparseable_timestamp"
	ReasonAmbiguousWrap    = "ambiguous_pts_wrap"
	ReasonDuplicateUnfix   = "unresolvable_duplicate_timestamp"
	ReasonUnrecoverable    = "unrecoverable_packet_loss"
	ReasonMissingFile      = "missing_segment_file"
	ReasonUnsafeURI        = "unsafe_segment_uri"
	ReasonInvalidTimescale = "invalid_timescale"
	ReasonLimitExceeded    = "limit_exceeded"
)

// Sentinel errors shared by the subsystems. Callers use errors.Is to map
// them to exit codes and diagnostics.
var (
	ErrLimitExceeded      = errors.New("media: input limit exceeded")
	ErrUnsafeURI          = errors.New("media: unsafe or remote URI")
	ErrPathTraversal      = errors.New("media: path traversal")
	ErrInvalidTimescale   = errors.New("media: invalid timescale")
	ErrTimelineTooLong    = errors.New("media: timeline exceeds 12h")
	ErrInvalidRange       = errors.New("media: invalid byte range")
	ErrNoManifest         = errors.New("media: no supported manifest found")
	ErrAmbiguousTimestamp = errors.New("media: ambiguous timestamp")
)

// FaultError adds structured context to a repair failure.
type FaultError struct {
	Kind   error
	Detail string
}

func (e *FaultError) Error() string { return e.Kind.Error() + ": " + e.Detail }
func (e *FaultError) Unwrap() error { return e.Kind }

// Sample is one timestamped media unit.
type Sample struct {
	PTS       Rational `json:"pts"`
	Duration  Rational `json:"duration"`
	Sync      bool     `json:"sync"`
	Raw       int64    `json:"raw"`   // raw clock value (ticks or unwrapped PTS)
	Scale     int64    `json:"scale"` // clock ticks per second
	Source    string   `json:"source"`
	Adjusted  int64    `json:"adjusted,omitempty"` // ticks added during repair
	Duplicate bool     `json:"duplicate,omitempty"`
}

// Segment is one referenced media segment plus its parsed samples.
type Segment struct {
	ID              string    `json:"id"`
	Track           string    `json:"track"`
	Kind            TrackType `json:"kind"`
	Source          string    `json:"source"` // hls or dash
	Seq             int       `json:"seq"`
	URI             string    `json:"uri"`
	File            string    `json:"file"`
	DeclaredStart   Rational  `json:"declared_start"`
	Duration        Rational  `json:"duration"`
	Discontinuity   bool      `json:"discontinuity,omitempty"`
	Timescale       int64     `json:"timescale"`
	ByteOffset      int64     `json:"byte_offset,omitempty"`
	ByteLength      int64     `json:"byte_length,omitempty"`
	Samples         []Sample  `json:"samples,omitempty"`
	RawStart        int64     `json:"raw_start,omitempty"`
	RawEnd          int64     `json:"raw_end,omitempty"`
	Format          string    `json:"format,omitempty"`
	SourceSHA       string    `json:"source_sha256,omitempty"`
	StateFile       string    `json:"state_file,omitempty"`
	Quarantine      string    `json:"quarantine,omitempty"`
	QuarantineCause string    `json:"quarantine_cause,omitempty"`
	MissingPackets  int       `json:"missing_packets,omitempty"`
	RecoveredPacket int       `json:"recovered_packets,omitempty"`
	UnrecoverGroups int       `json:"unrecovered_groups,omitempty"`
	Reconstructed   bool      `json:"reconstructed,omitempty"`
	Output          string    `json:"output,omitempty"`
	OutputSHA       string    `json:"output_sha256,omitempty"`
}

// Valid reports whether the segment may be referenced by the seek index.
func (s *Segment) Valid() bool { return s.Quarantine == "" && len(s.Samples) > 0 }

// Quarantine marks the segment as isolated while preserving its reason.
func (s *Segment) QuarantineWith(reason, detail string) {
	if s.Quarantine != "" {
		return
	}
	s.Quarantine = reason
	s.QuarantineCause = detail
	s.Samples = nil
}

// Track is one normalized timeline.
type Track struct {
	ID        string     `json:"id"`
	Kind      TrackType  `json:"kind"`
	Source    string     `json:"source"`
	Timescale int64      `json:"timescale"`
	Origin    Rational   `json:"origin"` // raw-to-timeline anchor
	Shift     Rational   `json:"shift"`  // applied A/V correction
	Segments  []*Segment `json:"segments"`
}

// TrackLen returns the declared timeline length of the track.
func (t *Track) TrackLen() Rational {
	out := Zero
	for _, s := range t.Segments {
		if s.Duration.Cmp(Zero) > 0 {
			out = out.Add(s.Duration)
		}
	}
	return out
}

// Correction records one timestamp repair decision.
type Correction struct {
	Segment string `json:"segment"`
	Track   string `json:"track"`
	Kind    string `json:"kind"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Detail  string `json:"detail,omitempty"`
}

// Quarantine is the durable reason record for an isolated segment.
type Quarantine struct {
	Segment string `json:"segment"`
	Track   string `json:"track"`
	URI     string `json:"uri"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"`
}

// Timeline is the merged, normalized presentation.
type Timeline struct {
	Base        Rational     `json:"base"`
	Tracks      []*Track     `json:"tracks"`
	Corrections []Correction `json:"corrections,omitempty"`
	Quarantine  []Quarantine `json:"quarantine,omitempty"`
	Warnings    []string     `json:"warnings,omitempty"`
}

// FindTrack returns the track with the given ID.
func (tl *Timeline) FindTrack(id string) *Track {
	for _, t := range tl.Tracks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// CheckTimelineLimit rejects timelines longer than MaxTimeline.
func CheckTimelineLimit(start, end Rational) error {
	if end.Sub(start).Seconds() > MaxTimeline.Seconds() {
		return &FaultError{Kind: ErrTimelineTooLong, Detail: end.Sub(start).String() + "s"}
	}
	return nil
}

// ResolveLocalURI resolves a manifest-relative segment URI while rejecting
// remote schemes, absolute paths, traversal and symlink escapes.
func ResolveLocalURI(baseDir, uri string) (string, error) {
	u := strings.TrimSpace(uri)
	if u == "" {
		return "", &FaultError{Kind: ErrUnsafeURI, Detail: "empty URI"}
	}
	if strings.ContainsRune(u, 0) || strings.Contains(u, "\\") {
		return "", &FaultError{Kind: ErrUnsafeURI, Detail: "control character in URI"}
	}
	lower := strings.ToLower(u)
	if strings.HasPrefix(lower, "//") || strings.Contains(u, "://") {
		return "", &FaultError{Kind: ErrUnsafeURI, Detail: "remote URI " + u}
	}
	if idx := strings.IndexByte(u, ':'); idx > 0 {
		// A scheme-looking prefix (http:, file:, data:) is never local.
		if scheme := lower[:idx]; isScheme(scheme) {
			return "", &FaultError{Kind: ErrUnsafeURI, Detail: "URI scheme " + scheme}
		}
	}
	if filepath.IsAbs(u) {
		return "", &FaultError{Kind: ErrUnsafeURI, Detail: "absolute URI " + u}
	}
	clean := filepath.Clean(u)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", &FaultError{Kind: ErrPathTraversal, Detail: u}
	}
	root, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	target := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", &FaultError{Kind: ErrPathTraversal, Detail: u}
	}
	return target, nil
}

func isScheme(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
				return false
			}
			continue
		}
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// CheckFileSize enforces a byte limit before reading a file.
func CheckFileSize(path string, limit int64) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, &FaultError{Kind: ErrLimitExceeded, Detail: path + " is not a regular file"}
	}
	if info.Size() > limit {
		return 0, &FaultError{Kind: ErrLimitExceeded, Detail: fmt.Sprintf("%s size %d > %d", path, info.Size(), limit)}
	}
	return info.Size(), nil
}

// ReadManifest reads a manifest after enforcing MaxManifestBytes.
func ReadManifest(path string) ([]byte, error) {
	if _, err := CheckFileSize(path, MaxManifestBytes); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// ReadSegment reads a segment after enforcing MaxSegmentBytes and any
// declared byte range. Ranges must lie fully inside the file.
func ReadSegment(path string, offset, length int64) ([]byte, error) {
	size, err := CheckFileSize(path, MaxSegmentBytes)
	if err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 {
		return nil, &FaultError{Kind: ErrInvalidRange, Detail: "negative offset or length"}
	}
	if length == 0 && offset == 0 {
		return os.ReadFile(path)
	}
	if offset > size || length > size-offset {
		return nil, &FaultError{Kind: ErrInvalidRange, Detail: fmt.Sprintf("%d+%d > %d", offset, length, size)}
	}
	buf := make([]byte, length)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.ReadAt(buf, offset); err != nil {
		return nil, err
	}
	return buf, nil
}
