// Package media defines the shared time base and error model used by the
// EBML parser, timeline merger, audio normalizer, indexer and DAG runner.
package media

// MediaTime is the pipeline-wide time base: signed nanoseconds relative to
// the segment epoch (the zero point of cluster timecodes). Negative values
// are legal at the type level; the timeline layer decides whether to accept
// or reject blocks carrying them.
type MediaTime int64

// Nanosecond scale constants.
const (
	NanosPerMicro  int64 = 1_000
	NanosPerMilli  int64 = 1_000_000
	NanosPerSecond int64 = 1_000_000_000
)

// DefaultTimestampScale is the Matroska default: one timecode unit is one
// millisecond, expressed in nanoseconds per unit.
const DefaultTimestampScale uint64 = 1_000_000

// FromNanoseconds wraps an explicit nanosecond offset.
func FromNanoseconds(ns int64) MediaTime { return MediaTime(ns) }

// FromMilli converts a millisecond timecode to the unified time base.
func FromMilli(ms int64) MediaTime { return MediaTime(ms * NanosPerMilli) }

// FromScaledTimecode converts a raw Matroska timecode using the segment's
// TimestampScale (nanoseconds per timecode unit). A zero scale falls back
// to the Matroska default rather than dividing by zero.
func FromScaledTimecode(scale uint64, timecode int64) MediaTime {
	if scale == 0 {
		scale = DefaultTimestampScale
	}
	return MediaTime(timecode * int64(scale))
}

// Nanoseconds returns the raw signed nanosecond value.
func (t MediaTime) Nanoseconds() int64 { return int64(t) }

// Milliseconds returns the value truncated to whole milliseconds.
func (t MediaTime) Milliseconds() int64 { return int64(t) / NanosPerMilli }

// RTPDelta returns the signed 32-bit difference (a-b), correctly handling
// sequence-number/timestamp wraparound as long as the true distance is
// within +/- 2^31 ticks.
func RTPDelta(a, b uint32) int64 {
	return int64(int32(a - b))
}

// FromRTPTimestamp maps an RTP timestamp onto the unified time base given
// one anchor point (baseRTP at baseTime) and the payload clock rate.
// Wraparound is handled by RTPDelta; the result is deterministic for any
// pair of timestamps within half the 32-bit clock range of the anchor.
func FromRTPTimestamp(clockRate uint32, baseRTP uint32, baseTime MediaTime, ts uint32) MediaTime {
	if clockRate == 0 {
		return baseTime
	}
	delta := RTPDelta(ts, baseRTP)
	return baseTime + MediaTime(delta*NanosPerSecond/int64(clockRate))
}
