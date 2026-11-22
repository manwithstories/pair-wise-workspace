package pipeline

import (
	"mediascan/internal/media"
	"mediascan/internal/segment"
	"mediascan/internal/stream"
	"mediascan/internal/subtitle"
)

// Limits bundles per-subsystem bounds.
type Limits struct {
	Subtitle subtitle.Limits
	Stream   stream.Limits
	RTP      stream.RTPLimits
}

// DefaultLimits are the safe defaults used by the CLI.
var DefaultLimits = Limits{
	Subtitle: subtitle.DefaultLimits,
	Stream:   stream.DefaultLimits,
	RTP:      stream.DefaultRTPLimits,
}

// Config controls a pipeline run.
type Config struct {
	// SegmentNs is the nominal timeline segment length.
	SegmentNs int64
	// CheckpointEvery commits a replay checkpoint every N segments.
	CheckpointEvery int
	// SubtitleTrackID is the timeline id for subtitle events when the input
	// does not override it.
	SubtitleTrackID uint32
	// VideoFrameRate selects the frame rate for media timecode rendering and
	// frame-alignment checks.
	VideoFrameRate media.FrameRate
	// Limits bound each subsystem.
	Limits Limits
}

func (c Config) normalize() Config {
	if c.SegmentNs <= 0 {
		c.SegmentNs = media.NanosPerSecond
	}
	if c.CheckpointEvery <= 0 {
		c.CheckpointEvery = 5
	}
	if c.SubtitleTrackID == 0 {
		c.SubtitleTrackID = 100
	}
	if c.VideoFrameRate.Num == 0 {
		c.VideoFrameRate = media.Rate2997
	}
	if c.Limits.Subtitle == (subtitle.Limits{}) {
		c.Limits.Subtitle = subtitle.DefaultLimits
	}
	if c.Limits.Stream == (stream.Limits{}) {
		c.Limits.Stream = stream.DefaultLimits
	}
	if c.Limits.RTP == (stream.RTPLimits{}) {
		c.Limits.RTP = stream.DefaultRTPLimits
	}
	return c
}

// Input bundles the in-memory assets to process.
type Input struct {
	// SubtitleBytes is an SRT or VTT document; nil skips subtitle processing.
	SubtitleBytes  []byte
	SubtitleFormat subtitle.Format
	// SubtitleTrackID overrides Config.SubtitleTrackID when non-zero.
	SubtitleTrackID uint32

	// MP4Bytes is an MP4 file; nil skips media processing.
	MP4Bytes []byte
	// RTPPackets is a sequence of serialized RTP packets in arrival order.
	RTPPackets [][]byte
	// RTPClockRate scales RTP timestamps to ns; zero leaves them unscaled.
	RTPClockRate uint32
}

// Segment is one final output segment with its SHA-256 digest.
type Segment struct {
	Index   int
	StartNs int64
	EndNs   int64
	// StartTimecode is the segment start rendered as SMPTE timecode at the
	// configured video frame rate, so editors can locate it on a deck.
	StartTimecode string
	// FrameAligned reports whether the start landed exactly on a frame
	// boundary; false flags a non-integral frame time.
	FrameAligned bool
	Recovery     segment.Recovery
	TrackIDs     []uint32
	Sources      []segment.Source
	Classes      []string
	Late         int
	Hash         string
	HashShort    string
}

// Output is the complete result of a pipeline run.
type Output struct {
	Segments []Segment
	Issues   media.IssueSet
	// Corrupt and Recovered locate damaged and successfully recovered spans.
	Corrupt   []media.Range
	Recovered []media.Range

	// SubtitleRoundtrip is 1.0 when every cue survives a round trip.
	SubtitleRoundtrip float64
	// KeyframeHitRate is the fraction of expected keyframes present.
	KeyframeHitRate float64
	// KeyframeIntervals are GOP gaps in ns (video track), for inspection.
	KeyframeIntervals []int64
	// TimecodeNonIntegral counts segment starts that were not frame aligned.
	TimecodeNonIntegral int
	// RebuiltIndexes counts tracks whose index had to be rebuilt.
	RebuiltIndexes int
	// ReplayEqual reports whether every late-event replay was exact.
	ReplayEqual bool
	// ReplayCount counts late-event replays.
	ReplayCount int

	// Input size counters for the demo header.
	Tracks     int
	Cues       int
	Packets    int
	LateEvents int
	Copies     int

	// Fatal reports whether processing could not complete.
	Fatal bool
}

// exitCode maps the output to the CLI contract: 0 success, 2 localized but
// recoverable faults, 1 unrecoverable failure or failed consistency check.
func (o *Output) ExitCode() int {
	if o.Fatal || o.Issues.Fatal() {
		return 1
	}
	if !o.ReplayEqual {
		return 1
	}
	if len(o.Corrupt) > 0 || len(o.Recovered) > 0 || o.Issues.RecoverableCount() > 0 {
		return 2
	}
	return 0
}
