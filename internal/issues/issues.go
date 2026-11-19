// Package issues defines the unified error model shared by every parser and
// inspector. Any record that cannot be converted is preserved verbatim (Raw)
// together with its exact location (Path, ByteOffset, Line) so a reviewer can
// trace output frames back to the source segment. An Issue never aborts the
// batch: the offending record is isolated and processing continues.
package issues

// Kind classifies an isolated record-level problem.
const (
	Overlap           = "overlap"
	InvalidTime       = "invalid-time"
	TruncatedRecord   = "truncated-record"
	MalformedRecord   = "malformed-record"
	TCUnconvertible   = "tc-unconvertible"
	RTPDuplicate      = "rtp-duplicate"
	RTPBackward       = "rtp-backward"
	RTPGap            = "rtp-gap"
	MP4Truncated      = "mp4-truncated"
	MP4IncompleteIdx  = "mp4-incomplete-index"
	MP4Unreadable     = "mp4-unreadable"
	CheckpointCorrupt = "checkpoint-corrupt"
)

// Issue is one isolated, located error record.
type Issue struct {
	Kind       string `json:"kind"`
	Path       string `json:"path,omitempty"`
	Track      string `json:"track,omitempty"`
	ByteOffset int64  `json:"byte_offset"`
	Line       int    `json:"line,omitempty"`
	Raw        string `json:"raw,omitempty"`
	Msg        string `json:"msg"`
}
