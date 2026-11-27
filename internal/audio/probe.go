// Package audio parses 48 kHz PCM calibration pulses and measures the
// offset between the audio clock and the video timeline.
package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"fieldcut/internal/media"
)

// Pulse describes a detected calibration pulse inside a PCM buffer.
type Pulse struct {
	Start int // first sample of the pulse
	End   int // one past the last sample
	Peak  int // index of the largest absolute sample
	PeakV float64
}

// Probe is the measured audio alignment.
type Probe struct {
	Rate          int
	Samples       int
	Pulses        []Pulse
	OffsetNanos   int64 // audio pulse 0 relative to the video frame-0 edge
	MaxErrorNanos int64
}

// ParsePCM converts interleaved 16-bit signed little-endian data into
// per-channel float64 samples. A short final sample is rejected.
func ParsePCM(data []byte, channels int) ([]float64, error) {
	if channels <= 0 || channels > 2 {
		return nil, media.Newf(media.KindInput, "audio.pcm", "", -1, "channels=%d outside [1,2]", channels)
	}
	if len(data) == 0 || len(data)%2 != 0 {
		return nil, media.New(media.KindInput, "audio.pcm", "", -1, errors.New("odd or empty PCM buffer"))
	}
	out := make([]float64, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		v := int16(binary.LittleEndian.Uint16(data[i : i+2]))
		out = append(out, float64(v)/32768.0)
	}
	return out, nil
}

// ParsePCMChannels parses interleaved PCM and returns per-channel series.
func ParsePCMChannels(data []byte, channels int) ([][]float64, error) {
	frames, err := ParsePCM(data, channels)
	if err != nil {
		return nil, err
	}
	if len(frames)%channels != 0 {
		return nil, media.Newf(media.KindInput, "audio.pcm", "", -1,
			"sample count %d is not a multiple of %d channels", len(frames), channels)
	}
	out := make([][]float64, channels)
	for c := range out {
		out[c] = make([]float64, len(frames)/channels)
	}
	for i, v := range frames {
		out[i%channels][i/channels] = v
	}
	return out, nil
}

// DetectPulses finds separated bursts above the threshold (absolute loudness)
// and returns them in sample order. A burst closer than minGap samples to the
// previous one is merged so a noisy pulse cannot split in two.
func DetectPulses(samples []float64, threshold float64, minGap int) ([]Pulse, error) {
	if len(samples) == 0 {
		return nil, media.New(media.KindInput, "audio.detect", "", -1, errors.New("no samples"))
	}
	if threshold <= 0 || threshold >= 1 {
		return nil, media.Newf(media.KindInput, "audio.detect", "", -1, "threshold=%v outside (0,1)", threshold)
	}
	if minGap < 0 {
		return nil, media.Newf(media.KindInput, "audio.detect", "", -1, "minGap=%d negative", minGap)
	}
	var bursts []Pulse
	cur := Pulse{Start: -1, PeakV: 0}
	for i, v := range samples {
		a := math.Abs(v)
		if a >= threshold {
			if cur.Start < 0 {
				cur = Pulse{Start: i, Peak: i, PeakV: a}
			}
			if a > cur.PeakV {
				cur.Peak, cur.PeakV = i, a
			}
			cur.End = i + 1
			continue
		}
		if cur.Start >= 0 {
			if i-cur.End >= minGap || i == len(samples)-1 {
				bursts = append(bursts, cur)
				cur = Pulse{Start: -1, PeakV: 0}
			}
		}
	}
	if cur.Start >= 0 {
		bursts = append(bursts, cur)
	}
	return bursts, nil
}

// ProbeAlignment measures the first pulse edge against the expected video
// edge. expectedNanos is when pulse 0 should start; offset is measured minus
// expected. MaxErrorNanos is the largest per-pulse deviation from the linear
// pulse grid, which proves audio and picture share a clock.
func ProbeAlignment(samples []float64, rate int, expectedNanos int64, threshold float64, minGap int) (Probe, error) {
	if rate != media.AudioRate {
		return Probe{}, media.Newf(media.KindInput, "audio.probe", "", -1, "rate=%d, want %d", rate, media.AudioRate)
	}
	pulses, err := DetectPulses(samples, threshold, minGap)
	if err != nil {
		return Probe{}, err
	}
	if len(pulses) < 2 {
		return Probe{}, media.Newf(media.KindInput, "audio.probe", "", -1,
			"need at least two pulses, found %d", len(pulses))
	}
	sampleNanos := int64(1e9) / int64(rate)
	off := int64(pulses[0].Start)*sampleNanos - expectedNanos
	// The pulse sequence must lie on one grid. Compare each pulse against
	// the best-fit period; any drift beyond a sample proves desync.
	spacing := float64(pulses[len(pulses)-1].Start-pulses[0].Start) / float64(len(pulses)-1)
	maxErr := 0.0
	for i, pl := range pulses {
		want := float64(pulses[0].Start) + spacing*float64(i)
		if d := math.Abs(float64(pl.Start) - want); d > maxErr {
			maxErr = d
		}
	}
	maxErrNanos := int64(math.Round(maxErr * float64(sampleNanos)))
	return Probe{
		Rate:          rate,
		Samples:       len(samples),
		Pulses:        pulses,
		OffsetNanos:   off,
		MaxErrorNanos: maxErrNanos,
	}, nil
}

// OffsetMs renders the measured offset in milliseconds.
func (p Probe) OffsetMs() float64 { return float64(p.OffsetNanos) / 1e6 }

// Aligned reports whether the audio offset is within a reasonable tolerance
// for a 25 fps output grid (half a frame).
func (p Probe) Aligned() bool {
	half := float64(1e9/media.FrameRate) / 2
	return math.Abs(float64(p.OffsetNanos)) <= half
}

func (p Probe) String() string {
	return fmt.Sprintf("pulses=%d offset_ms=%.3f max_err_ms=%.3f", len(p.Pulses), p.OffsetMs(), float64(p.MaxErrorNanos)/1e6)
}

// SyntheticPCM renders a deterministic calibration buffer: flat pulses of the
// given width, spaced every period samples, starting at startSample. The
// leading edge of each pulse is its time instant; no external audio is used.
func SyntheticPCM(samples, rate, startSample, period, width int, amplitude float64) ([]byte, error) {
	if samples <= 0 || samples > (1<<24) {
		return nil, media.Newf(media.KindLimit, "audio.synth", "", -1, "samples=%d outside (0,2^24]", samples)
	}
	if period <= 0 || startSample < 0 || startSample >= samples {
		return nil, media.Newf(media.KindInput, "audio.synth", "", -1, "bad pulse geometry")
	}
	buf := make([]float64, samples)
	for p := startSample; p < samples; p += period {
		for i := 0; i < width && p+i < samples; i++ {
			// A flat pulse keeps the leading edge exact: detection onset,
			// peak and sample index coincide, so alignment is measurable.
			buf[p+i] = amplitude
		}
	}
	out := make([]byte, samples*2)
	for i, v := range buf {
		s := int16(math.Round(v * 32767))
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return out, nil
}
