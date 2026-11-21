package audio

import (
	"fmt"

	"mediamerge/internal/clock"
	"mediamerge/internal/fault"
)

// ResampleError reports a sample-rate conversion that cannot be expressed as
// whole output frames. The track and its timebase are part of the message so
// an operator can find the segment without replaying the run.
type ResampleError struct {
	TrackID  int
	Timebase clock.Timebase
	InRate   int
	OutRate  int
	Frames   int
}

func (e *ResampleError) Error() string {
	return fmt.Sprintf("audio: track %d (%s): %d frames at %d Hz cannot be resampled to %d Hz without a partial frame",
		e.TrackID, e.Timebase, e.Frames, e.InRate, e.OutRate)
}

// Resample converts interleaved int32 PCM from inRate to outRate using exact
// rational linear interpolation. The output frame count must be an integer;
// otherwise the conversion fails rather than truncating a frame. Channels are
// carried through unchanged (layout conversion is a separate, explicit step).
func Resample(trackID int, samples []int32, channels, inRate, outRate int, tb clock.Timebase) ([]int32, error) {
	if channels <= 0 {
		return nil, fault.NewTrack(fault.StageAudio, fault.CodeInvalidArgument, trackID,
			fmt.Sprintf("invalid channel count %d", channels))
	}
	if inRate <= 0 || outRate <= 0 {
		return nil, fault.NewTrack(fault.StageAudio, fault.CodeInvalidArgument, trackID,
			fmt.Sprintf("invalid sample rates %d -> %d", inRate, outRate))
	}
	if len(samples)%channels != 0 {
		span, _ := SampleSpan(trackID, 0, len(samples), inRate, tb)
		return nil, fault.NewSpan(fault.StageAudio, fault.CodeSampleIndivisible, trackID, span,
			fmt.Sprintf("%d samples do not form whole %d-channel frames", len(samples), channels))
	}
	inFrames := len(samples) / channels
	if inRate == outRate {
		out := make([]int32, len(samples))
		copy(out, samples)
		return out, nil
	}

	num := int64(inFrames) * int64(outRate)
	if num%int64(inRate) != 0 {
		reason := &ResampleError{TrackID: trackID, Timebase: tb, InRate: inRate, OutRate: outRate, Frames: inFrames}
		span := clock.Span{
			Start: clock.MustRational(0, 1),
			End:   clock.MustRational(int64(inFrames), int64(inRate)),
		}
		fe := fault.Wrap(fault.StageAudio, fault.CodeSampleIndivisible, reason,
			fmt.Sprintf("track %d %s: %d frames at %d Hz -> %d Hz", trackID, tb, inFrames, inRate, outRate))
		fe.TrackID = trackID
		fe.Span = &span
		return nil, fe
	}
	outFrames := num / int64(inRate)
	out := make([]int32, int(outFrames)*channels)

	for j := int64(0); j < outFrames; j++ {
		// Source position in frames: pos = j*inRate/outRate.
		posNum := j * int64(inRate)
		i := posNum / int64(outRate)
		rem := posNum % int64(outRate) // fraction of the way to i+1
		next := i + 1
		if next >= int64(inFrames) {
			next = int64(inFrames) - 1
		}
		for c := 0; c < channels; c++ {
			a := int64(samples[i*int64(channels)+int64(c)])
			b := int64(samples[next*int64(channels)+int64(c)])
			// (a*(outRate-rem) + b*rem) / outRate, rounded half away from zero.
			v := a*(int64(outRate)-rem) + b*rem
			if v >= 0 {
				v = (v + int64(outRate)/2) / int64(outRate)
			} else {
				v = (v - int64(outRate)/2) / int64(outRate)
			}
			out[j*int64(channels)+int64(c)] = clampInt32(v)
		}
	}
	return out, nil
}

func clampInt32(v int64) int32 {
	const max = int64(1<<31 - 1)
	const min = -max - 1
	if v > max {
		return int32(max)
	}
	if v < min {
		return int32(min)
	}
	return int32(v)
}

// ResampleSpan is the output duration of a resampled buffer, used for the
// stage digest and segment boundaries.
func ResampleSpan(frames, rate int) clock.Span {
	return clock.Span{Start: clock.MustRational(0, 1), End: clock.MustRational(int64(frames), int64(rate))}
}
