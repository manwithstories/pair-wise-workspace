package model

import "fmt"

// ManifestKind identifies which index format produced a segment reference.
type ManifestKind string

// Supported manifest kinds.
const (
	KindHLS  ManifestKind = "hls"
	KindDASH ManifestKind = "dash"
)

// TrackRole distinguishes the three ingestion sources of the demo pipeline.
type TrackRole string

// Supported track roles.
const (
	TrackStudio TrackRole = "studio"    // 棚内
	TrackField  TrackRole = "field"     // 外景
	TrackVoice  TrackRole = "voiceover" // 远端解说
)

// SegmentRef is one confirmed fragment on the unified timeline.
type SegmentRef struct {
	Track    TrackRole    `json:"track"`
	Kind     ManifestKind `json:"kind"`
	Index    int          `json:"index"`
	URI      string       `json:"uri"`
	Start    Rat          `json:"-"`
	Duration Rat          `json:"-"`
	// StartMS / DurationMS are the JSON-stable projection of the rational span.
	StartMS    int64 `json:"start_ms"`
	DurationMS int64 `json:"duration_ms"`
	// ByteStart/ByteSize locate the fragment inside its file (0/0 = whole file).
	ByteStart int64 `json:"byte_start"`
	ByteSize  int64 `json:"byte_size"`
	Timescale int64 `json:"timescale"`
	Sync      bool  `json:"sync"`
}

// End returns the exclusive timeline end of the segment.
func (s SegmentRef) End() (Rat, error) { return s.Start.Add(s.Duration) }

// Timeline is the normalized, per-track ordered view of confirmed segments.
type Timeline struct {
	Tracks  map[TrackRole][]SegmentRef `json:"tracks"`
	Missing []MissingDependency        `json:"missing,omitempty"`
	Partial bool                       `json:"partial"`
}

// NewTimeline creates an empty timeline.
func NewTimeline() *Timeline {
	return &Timeline{Tracks: map[TrackRole][]SegmentRef{}}
}

// Add appends a segment, rejecting negative or zero-length durations.
func (t *Timeline) Add(seg SegmentRef) error {
	if seg.Duration.num <= 0 {
		return fmt.Errorf("%w: segment %s#%d", ErrNegativeDuration, seg.Track, seg.Index)
	}
	if seg.ByteStart < 0 || seg.ByteSize < 0 {
		return fmt.Errorf("%w: negative byte range on %s#%d", ErrNegativeDuration, seg.Track, seg.Index)
	}
	ms, err := seg.Start.Millis()
	if err != nil {
		return err
	}
	dms, err := seg.Duration.Millis()
	if err != nil {
		return err
	}
	seg.StartMS, seg.DurationMS = ms, dms
	t.Tracks[seg.Track] = append(t.Tracks[seg.Track], seg)
	return nil
}

// MarkPartial records the first unresolvable dependency for a track.
func (t *Timeline) MarkPartial(m MissingDependency) {
	t.Partial = true
	t.Missing = append(t.Missing, m)
}

// MissingDependency names the first index entry whose payload is unavailable.
type MissingDependency struct {
	Track  TrackRole    `json:"track"`
	Kind   ManifestKind `json:"kind"`
	Index  int          `json:"index"`
	URI    string       `json:"uri"`
	Reason string       `json:"reason"`
}

// MediaError carries the full provenance of a fragment-level failure.
type MediaError struct {
	Track       TrackRole
	SegmentIdx  int
	Offset      int64 // byte offset of the first bad byte / box header
	Recoverable bool
	Op          string
	Err         error
}

func (e *MediaError) Error() string {
	return fmt.Sprintf("media error op=%s track=%s segment=%d offset=%d recoverable=%v: %v",
		e.Op, e.Track, e.SegmentIdx, e.Offset, e.Recoverable, e.Err)
}

// Unwrap supports errors.Is/As on the underlying cause.
func (e *MediaError) Unwrap() error { return e.Err }

// NewMediaError is the single constructor so every failure carries provenance.
func NewMediaError(op string, track TrackRole, idx, offset int64, recoverable bool, cause error) *MediaError {
	return &MediaError{Op: op, Track: track, SegmentIdx: int(idx), Offset: offset,
		Recoverable: recoverable, Err: cause}
}

// Domain error sentinels.
var (
	// ErrNegativeDuration is returned for negative or non-positive durations.
	ErrNegativeDuration = fmt.Errorf("negative or zero duration")
	// ErrTruncated marks an input ending before its declared structure.
	ErrTruncated = fmt.Errorf("truncated segment")
	// ErrBoxOutOfRange marks a box whose declared size exceeds the file.
	ErrBoxOutOfRange = fmt.Errorf("box length out of range")
	// ErrBadSegment marks undecodable fragment payloads.
	ErrBadSegment = fmt.Errorf("bad segment payload")
	// ErrBoundaryUncertain means no safe keyframe cut point exists.
	ErrBoundaryUncertain = fmt.Errorf("keyframe boundary uncertain")
	// ErrChecksumMismatch invalidates checkpointed node outputs.
	ErrChecksumMismatch = fmt.Errorf("input digest changed")
	// ErrBlocked marks a node skipped because an upstream node failed.
	ErrBlocked = fmt.Errorf("blocked by upstream failure")
)
