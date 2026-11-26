package audio

import (
	"math"

	"pairflow/internal/media"
)

// ClipMix is one normalized clip laid onto the canonical output timeline.
// DissolveIn/Out are cross-fade lengths in canonical frames; an equal-power
// law keeps the summed energy constant across a dissolve.
type ClipMix struct {
	Span        Span
	Source      Source
	Label       string
	DissolveIn  int64
	DissolveOut int64
}

// MixClips sums clips into dst (48 kHz, 6 ch, frames [outStart, outStart+len)).
// Clips may overlap during a dissolve; outside their span they contribute
// nothing. trackGain scales the whole track before mixing.
func MixClips(dst []int16, outStart int64, clips []ClipMix, trackGain float64) *media.Error {
	n := int64(len(dst) / 6)
	for ci := range clips {
		c := &clips[ci]
		m, err := PlanMatrix(c.Source.Channels, c.Label)
		if err != nil {
			return err
		}
		srcFrame := make([]float64, c.Source.Channels)
		mixed := make([]float64, 6)
		for i := int64(0); i < n; i++ {
			j := outStart + i
			if j < c.Span.Start || j >= c.Span.End {
				continue
			}
			// Equal-power fades at the clip's own edges.
			g := trackGain
			if c.DissolveIn > 0 && j < c.Span.Start+c.DissolveIn {
				p := float64(j-c.Span.Start) / float64(c.DissolveIn)
				g *= math.Sin(p * math.Pi / 2)
			}
			if c.DissolveOut > 0 && j >= c.Span.End-c.DissolveOut {
				p := float64(c.Span.End-j) / float64(c.DissolveOut)
				g *= math.Sin(p * math.Pi / 2)
			}
			pos := float64(j) / float64(media.AudioRate) * float64(c.Source.Rate)
			for ch := 0; ch < c.Source.Channels; ch++ {
				srcFrame[ch] = c.Source.At(ch, pos)
			}
			m.MixFrame(mixed, srcFrame)
			base := i * 6
			for ch := 0; ch < 6; ch++ {
				v := float64(dst[base+int64(ch)]) + mixed[ch]*g*32767
				if v > 32767 {
					v = 32767
				} else if v < -32768 {
					v = -32768
				}
				dst[base+int64(ch)] = int16(math.Round(v))
			}
		}
	}
	return nil
}

// PeakAbs returns the largest absolute sample value, used by the verifier to
// confirm the normalized mix is audible and not clipped to silence.
func PeakAbs(pcm []int16) int {
	peak := 0
	for _, s := range pcm {
		v := int(s)
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	return peak
}

// RMS returns the root-mean-square level of the interleaved samples.
func RMS(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, s := range pcm {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(pcm)))
}
