// Package sync measures and corrects audio/video skew against a common
// time base, using wrap-safe timestamp arithmetic.
package sync

import "mediaroute/internal/model"

// Pair is one matched audio/video observation: raw RTP timestamps.
type Pair struct {
	Audio uint32 // audio sample clock (AudioRate Hz)
	Video uint32 // video PTS clock (VideoRate Hz)
}

// Alignment is the outcome of an Align call.
type Alignment struct {
	MeasuredMs   int64 // signed skew (audio - video) before correction
	CorrectionMs int64 // correction to apply to audio; 0 within threshold
	ResidualMs   int64 // skew remaining after correction
}

// Aligner maps both tracks onto a common millisecond base. The Base
// fields give the known mapping of one raw timestamp per track to the
// common base (as RTCP sender reports would provide).
type Aligner struct {
	ThresholdMs int64
	AudioRate   uint32 // e.g. 48000
	VideoRate   uint32 // e.g. 90000
	AudioBase   uint32
	AudioBaseMs int64
	VideoBase   uint32
	VideoBaseMs int64
}

// wrapDiff is a wrap-safe signed difference of 32-bit timestamps.
func wrapDiff(a, b uint32) int64 { return int64(int32(a - b)) }

// AudioMs maps an audio sample-clock timestamp to the common base.
func (a *Aligner) AudioMs(ts uint32) int64 {
	return a.AudioBaseMs + wrapDiff(ts, a.AudioBase)*1000/int64(a.AudioRate)
}

// VideoMs maps a video PTS timestamp to the common base.
func (a *Aligner) VideoMs(ts uint32) int64 {
	return a.VideoBaseMs + wrapDiff(ts, a.VideoBase)*1000/int64(a.VideoRate)
}

// Align computes the average skew across matched pairs. A skew beyond
// ThresholdMs yields a correction; timestamp wraparound is never
// treated as a jump.
func (a *Aligner) Align(pairs []Pair) Alignment {
	if len(pairs) == 0 {
		return Alignment{}
	}
	var sum int64
	for _, p := range pairs {
		sum += a.AudioMs(p.Audio) - a.VideoMs(p.Video)
	}
	skew := sum / int64(len(pairs))
	al := Alignment{MeasuredMs: skew, ResidualMs: skew}
	if skew > a.ThresholdMs || skew < -a.ThresholdMs {
		al.CorrectionMs = -skew
		al.ResidualMs = 0
	}
	return al
}

// AlignError wraps a lower-layer failure with sync-stage context while
// preserving the original error identity.
func AlignError(err error, msg string) error {
	if me, ok := err.(*model.MediaError); ok {
		return me.With("sync", msg)
	}
	return err
}
