// Package report defines the unified JSON report structures and the stable
// error codes shared by every sub-command.
package report

import "encoding/json"

// Stable process exit codes. Keep in sync with main.go.
const (
	ExitOK           = 0
	ExitArgs         = 2
	ExitFileRead     = 3
	ExitBoxInvalid   = 4
	ExitSeekOOB      = 5
	ExitHashMismatch = 6
)

// Error wraps a stable error code so the CLI can map failures to exit codes.
type Error struct {
	Code    int
	Op      string
	Message string
}

func (e *Error) Error() string {
	if e.Op == "" {
		return e.Message
	}
	return e.Op + ": " + e.Message
}

// NewError builds a coded error.
func NewError(code int, op, msg string) *Error {
	return &Error{Code: code, Op: op, Message: msg}
}

// ProbeTrack is the per-track summary emitted by probe.
type ProbeTrack struct {
	TrackID         uint32 `json:"track_id"`
	Handler         string `json:"handler"`
	Timescale       uint32 `json:"timescale"`
	SampleCount     int    `json:"sample_count"`
	KeyframeCount   int    `json:"keyframe_count"`
	FirstPtsMs      int64  `json:"first_pts_ms"`
	LastPtsMs       int64  `json:"last_pts_ms"`
	PresentationMs  int64  `json:"presentation_duration_ms"`
	MediaDurationMs int64  `json:"media_duration_ms"`
	WrapEvents      int    `json:"wrap_events"`
}

// Probe is the top-level probe payload.
type Probe struct {
	Tracks                 int          `json:"tracks"`
	Video                  *ProbeTrack  `json:"video,omitempty"`
	Audio                  *ProbeTrack  `json:"audio,omitempty"`
	AllTracks              []ProbeTrack `json:"all_tracks"`
	PresentationDurationMs int64        `json:"presentation_duration_ms"`
	MediaTimescale         uint32       `json:"movie_timescale"`
	UnknownBoxes           int          `json:"unknown_boxes_skipped"`
}

// SeekLanding describes a single seek result.
type SeekLanding struct {
	SampleIndex int    `json:"sample_index"`
	PtsMs       int64  `json:"pts_ms"`
	IsSync      bool   `json:"is_sync"`
	Offset      int64  `json:"keyframe_offset"`
	Size        uint32 `json:"size"`
	FrameType   string `json:"frame_type"`
}

// SeekResult is the payload of the seek command.
type SeekResult struct {
	RequestedMs   int64       `json:"requested_ms"`
	Landing       SeekLanding `json:"landing"`
	GopDistanceMs int64       `json:"gop_distance_ms"`
}

// HashResult is the payload of the hash command.
type HashResult struct {
	Frames      int    `json:"frames"`
	FirstSha256 string `json:"first_sha256"`
	RootSha256  string `json:"root_sha256"`
	Bytes       int64  `json:"bytes"`
}

// VerifyResult is the payload of the verify command.
type VerifyResult struct {
	Matched    int    `json:"matched"`
	Mismatched int    `json:"mismatched"`
	Status     string `json:"status"`
}

// Marshal renders any report value as indented JSON.
func Marshal(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}
