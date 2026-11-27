// Package media defines the shared microsecond time base, track
// identifiers and recoverable error types used across the archiver.
//
// All modules (RTP reordering, clock model, timeline, audio, video,
// archive) express media time on this single time base so that
// results are comparable and reproducible.
package media

import "fmt"

// TimebaseHz is the global media time base: one tick per microsecond.
const TimebaseHz = 1_000_000

// TimeUs is a media timestamp in microseconds on the shared timeline.
type TimeUs int64

// TrackKind distinguishes elementary streams.
type TrackKind int

const (
	TrackAudio TrackKind = iota
	TrackVideo
)

func (k TrackKind) String() string {
	switch k {
	case TrackAudio:
		return "audio"
	case TrackVideo:
		return "video"
	default:
		return "unknown"
	}
}

// TrackID identifies one RTP source within an archive run.
type TrackID struct {
	Kind TrackKind
	SSRC uint32
}

func (t TrackID) String() string {
	return fmt.Sprintf("%s/%08x", t.Kind, t.SSRC)
}

// PacketError is a recoverable stream anomaly. Offset is the absolute
// byte offset of the first anomalous byte in the input stream, so a
// reviewer can locate the defect without re-parsing.
type PacketError struct {
	Offset int64
	Reason string
	// Fatal marks framing errors (bad length prefix, truncated
	// record) after which the stream cannot be resynchronized.
	Fatal bool
}

func (e *PacketError) Error() string {
	return fmt.Sprintf("offset %d: %s", e.Offset, e.Reason)
}

// Quarantine records an event that arrived beyond the rollback horizon
// and was isolated instead of being merged into the timeline.
type Quarantine struct {
	Track  TrackID
	TimeUs TimeUs
	Reason string
}
