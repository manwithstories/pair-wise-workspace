// Package audio performs deterministic PCM resampling, channel mapping,
// metering and configurable silence detection.
package audio

import (
	"encoding/binary"
	"math"

	"fieldcut/internal/clock"
)

// Layout names a channel layout.
type Layout string

const (
	LayoutMono   Layout = "mono"
	LayoutStereo Layout = "stereo"
	Layout51     Layout = "5.1"
)

const MaxPCMBytes = 32 << 20

// PCM stores deinterleaved normalized channels in [-1,1].
type PCM struct {
	TrackID    int         `json:"track_id"`
	SampleRate int         `json:"sample_rate"`
	Layout     Layout      `json:"layout"`
	Channels   [][]float64 `json:"-"`
}

// Metrics are the reported audio measurements.
type Metrics struct {
	RMS          float64 `json:"rms"`
	RMSDBFS      float64 `json:"rms_dbfs"`
	Peak         float64 `json:"peak"`
	PeakDBFS     float64 `json:"peak_dbfs"`
	Clipped      int     `json:"clipped_samples"`
	SampleFrames int     `json:"sample_frames"`
}

func ChannelCount(layout Layout) int {
	switch layout {
	case LayoutMono:
		return 1
	case LayoutStereo:
		return 2
	case Layout51:
		return 6
	default:
		return 0
	}
}

// ReadPCM16 decodes signed little-endian interleaved PCM.
func ReadPCM16(trackID, sampleRate int, layout Layout, data []byte) (*PCM, error) {
	ch := ChannelCount(layout)
	if sampleRate <= 0 || ch == 0 {
		return nil, clock.Errorf(clock.CodeConfig, trackID, "unsupported PCM format rate=%d layout=%q", sampleRate, layout)
	}
	if len(data) > MaxPCMBytes {
		return nil, clock.Errorf(clock.CodeConfig, trackID, "PCM payload exceeds cap: %d > %d", len(data), MaxPCMBytes)
	}
	frameBytes := 2 * ch
	if frameBytes == 0 || len(data)%frameBytes != 0 {
		return nil, clock.Errorf(clock.CodeInvalidArgument, trackID,
			"PCM length %d is not frame aligned for %s", len(data), layout)
	}
	frames := len(data) / frameBytes
	out := &PCM{TrackID: trackID, SampleRate: sampleRate, Layout: layout, Channels: make([][]float64, ch)}
	for c := range out.Channels {
		out.Channels[c] = make([]float64, frames)
	}
	for i := 0; i < frames; i++ {
		for c := 0; c < ch; c++ {
			v := int16(binary.LittleEndian.Uint16(data[(i*ch+c)*2:]))
			out.Channels[c][i] = float64(v) / 32768.0
		}
	}
	return out, nil
}

// AppendPCM16 encodes PCM as signed 16-bit little-endian interleaved bytes.
func AppendPCM16(p *PCM) []byte {
	ch := len(p.Channels)
	if ch == 0 {
		return nil
	}
	frames := len(p.Channels[0])
	b := make([]byte, 0, frames*ch*2)
	for i := 0; i < frames; i++ {
		for c := 0; c < ch; c++ {
			v := p.Channels[c][i]
			if v > 1 {
				v = 1
			} else if v < -1 {
				v = -1
			}
			u := uint16(int16(math.Round(v * 32767)))
			b = append(b, byte(u), byte(u>>8))
		}
	}
	return b
}

// MapChannels applies an explicit deterministic channel matrix.
func MapChannels(p *PCM, target Layout) (*PCM, error) {
	in := ChannelCount(p.Layout)
	outCh := ChannelCount(target)
	if in == 0 || outCh == 0 || len(p.Channels) != in {
		return nil, clock.Errorf(clock.CodeConfig, p.TrackID, "bad channel layout %q", p.Layout)
	}
	frames := 0
	if in > 0 {
		frames = len(p.Channels[0])
	}
	q := &PCM{TrackID: p.TrackID, SampleRate: p.SampleRate, Layout: target, Channels: make([][]float64, outCh)}
	for c := range q.Channels {
		q.Channels[c] = make([]float64, frames)
	}
	for i := 0; i < frames; i++ {
		v := make([]float64, in)
		for c := range v {
			v[c] = p.Channels[c][i]
		}
		o := mapFrame(p.Layout, target, v)
		for c := 0; c < outCh; c++ {
			q.Channels[c][i] = o[c]
		}
	}
	return q, nil
}

func mapFrame(from, to Layout, v []float64) []float64 {
	mono := 0.0
	switch from {
	case LayoutMono:
		mono = v[0]
	case LayoutStereo:
		mono = 0.5*v[0] + 0.5*v[1]
	case Layout51: // L R C LFE Ls Rs
		mono = 0.25*v[0] + 0.25*v[1] + 0.25*v[2] + 0.125*v[4] + 0.125*v[5]
	}
	switch to {
	case LayoutMono:
		return []float64{mono}
	case LayoutStereo:
		switch from {
		case LayoutMono:
			return []float64{v[0], v[0]}
		case LayoutStereo:
			return []float64{v[0], v[1]}
		default:
			l := v[0] + 0.707*v[2] + 0.707*v[4] + 0.5*v[3]
			r := v[1] + 0.707*v[2] + 0.707*v[5] + 0.5*v[3]
			return []float64{l, r}
		}
	default: // 5.1: L R C LFE Ls Rs
		switch from {
		case LayoutMono:
			return []float64{0, 0, v[0], 0, 0, 0}
		case LayoutStereo:
			return []float64{v[0], v[1], 0.5 * (v[0] + v[1]), 0, 0, 0}
		default:
			return append([]float64(nil), v...)
		}
	}
}

// Resample converts to targetRate using linear interpolation. The algorithm is
// allocation-bounded and stable for the deterministic fixtures.
func Resample(p *PCM, targetRate int) (*PCM, error) {
	if targetRate <= 0 {
		return nil, clock.Errorf(clock.CodeConfig, p.TrackID, "target sample rate must be positive")
	}
	if p.SampleRate == targetRate {
		return p, nil
	}
	ch := len(p.Channels)
	inFrames := len(p.Channels[0])
	outFrames := int(float64(inFrames) * float64(targetRate) / float64(p.SampleRate))
	if outFrames < 1 {
		outFrames = 1
	}
	q := &PCM{TrackID: p.TrackID, SampleRate: targetRate, Layout: p.Layout, Channels: make([][]float64, ch)}
	ratio := float64(p.SampleRate) / float64(targetRate)
	for c := 0; c < ch; c++ {
		q.Channels[c] = make([]float64, outFrames)
		for i := 0; i < outFrames; i++ {
			pos := float64(i) * ratio
			idx := int(pos)
			frac := pos - float64(idx)
			if idx >= inFrames-1 {
				q.Channels[c][i] = p.Channels[c][inFrames-1]
				continue
			}
			q.Channels[c][i] = p.Channels[c][idx]*(1-frac) + p.Channels[c][idx+1]*frac
		}
	}
	return q, nil
}

// Analyze computes RMS/peak meters and full-scale clipped sample count.
func Analyze(p *PCM) Metrics {
	frames := 0
	if len(p.Channels) > 0 {
		frames = len(p.Channels[0])
	}
	var sumSquares, peak float64
	clipped := 0
	const clipAt = 32767.0 / 32768.0
	for _, ch := range p.Channels {
		for _, v := range ch {
			av := math.Abs(v)
			sumSquares += v * v
			if av > peak {
				peak = av
			}
			if av >= clipAt {
				clipped++
			}
		}
	}
	n := float64(frames * len(p.Channels))
	rms := 0.0
	if n > 0 {
		rms = math.Sqrt(sumSquares / n)
	}
	return Metrics{
		RMS: rms, RMSDBFS: dbFS(rms), Peak: peak, PeakDBFS: dbFS(peak),
		Clipped: clipped, SampleFrames: frames,
	}
}

func dbFS(v float64) float64 {
	if v < 1e-7 {
		return -120
	}
	return 20 * math.Log10(v)
}

// SilenceIntervals returns runs whose maximum channel amplitude stays below
// thresholdDBFS for at least minDuration. Intervals are half-open nanoseconds.
func SilenceIntervals(p *PCM, thresholdDBFS float64, minDuration clock.Ns) []clock.Interval {
	threshold := math.Pow(10, thresholdDBFS/20)
	frames := 0
	if len(p.Channels) > 0 {
		frames = len(p.Channels[0])
	}
	frameSilent := make([]bool, frames)
	for i := 0; i < frames; i++ {
		maxAmp := 0.0
		for _, ch := range p.Channels {
			if a := math.Abs(ch[i]); a > maxAmp {
				maxAmp = a
			}
		}
		frameSilent[i] = maxAmp < threshold
	}
	var out []clock.Interval
	start := -1
	flush := func(endFrame int) {
		if start < 0 {
			return
		}
		startNs, _ := clock.SamplesToNs(int64(start), p.SampleRate)
		endNs, _ := clock.SamplesToNs(int64(endFrame), p.SampleRate)
		iv := clock.Interval{Start: startNs, End: endNs}
		if iv.Duration() >= minDuration {
			out = append(out, iv)
		}
		start = -1
	}
	for i, silent := range frameSilent {
		if silent && start < 0 {
			start = i
		} else if !silent && start >= 0 {
			flush(i)
		}
	}
	if start >= 0 {
		flush(frames)
	}
	return out
}

// Process maps channels, resamples and analyzes the result.
func Process(p *PCM, targetRate int, target Layout, thresholdDB float64, minSilence clock.Ns) (*PCM, Metrics, []clock.Interval, error) {
	mapped, err := MapChannels(p, target)
	if err != nil {
		return nil, Metrics{}, nil, err
	}
	resampled, err := Resample(mapped, targetRate)
	if err != nil {
		return nil, Metrics{}, nil, err
	}
	return resampled, Analyze(resampled), SilenceIntervals(resampled, thresholdDB, minSilence), nil
}
