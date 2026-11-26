package audio

import (
	"math"

	"mediapipe/internal/media"
)

// Options configures RMS silence detection. Thresholds are dBFS: more negative
// values require a quieter signal. Enter must be quieter/lower than Exit; the
// band between them is the hysteresis hold region.
type Options struct {
	SampleRate   int     `json:"sample_rate"`
	Channels     int     `json:"channels"`
	WindowMS     int     `json:"window_ms"`
	EnterDBFS    float64 `json:"enter_dbfs"`
	ExitDBFS     float64 `json:"exit_dbfs"`
	MinSilenceMS int     `json:"min_silence_ms"`
}

func DefaultOptions() Options {
	return Options{SampleRate: 48_000, Channels: 1, WindowMS: 20, EnterDBFS: -50, ExitDBFS: -42, MinSilenceMS: 250}
}

func (o Options) normalize() Options {
	d := DefaultOptions()
	if o.SampleRate <= 0 {
		o.SampleRate = d.SampleRate
	}
	if o.Channels <= 0 {
		o.Channels = 1
	}
	if o.WindowMS <= 0 {
		o.WindowMS = 20
	}
	if o.EnterDBFS == 0 {
		o.EnterDBFS = d.EnterDBFS
	}
	if o.ExitDBFS == 0 || o.ExitDBFS <= o.EnterDBFS {
		// Exit must be louder (less negative) than enter.
		o.ExitDBFS = o.EnterDBFS + 8
	}
	if o.MinSilenceMS <= 0 {
		o.MinSilenceMS = d.MinSilenceMS
	}
	return o
}

type Region struct {
	StartSec    float64 `json:"start_sec"`
	EndSec      float64 `json:"end_sec"`
	DurationSec float64 `json:"duration_sec"`
	Boundary    string  `json:"boundary,omitempty"` // start, end, both
}

type Report struct {
	Regions         []Region `json:"silence_regions"`
	PeakAmplitude   float64  `json:"peak_amplitude"`
	PeakDBFS        float64  `json:"peak_dbfs"`
	WindowCount     int      `json:"window_count"`
	ShortSuppressed int      `json:"short_suppressed"`
}

// Analyze computes peak and hysteresis silence regions over mono float samples
// in [-1,1]. Multi-channel input should already be downmixed.
func Analyze(samples []float64, opt Options) (Report, error) {
	opt = opt.normalize()
	if opt.SampleRate <= 0 {
		return Report{}, media.NewError(media.CodeInvalidInput, "sample rate must be positive", nil)
	}
	win := opt.SampleRate * opt.WindowMS / 1000
	if win <= 0 {
		win = 1
	}
	enterAmp := dbToAmplitude(opt.EnterDBFS)
	exitAmp := dbToAmplitude(opt.ExitDBFS)
	rep := Report{}
	var openStart float64 = -1
	open := false
	total := len(samples)
	for start := 0; start < total; start += win {
		end := start + win
		if end > total {
			end = total
		}
		if end-start == 0 {
			continue
		}
		sumSq := 0.0
		peak := 0.0
		for _, v := range samples[start:end] {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				v = 0
			}
			a := math.Abs(v)
			if a > peak {
				peak = a
			}
			sumSq += v * v
		}
		rms := math.Sqrt(sumSq / float64(end-start))
		if peak > rep.PeakAmplitude {
			rep.PeakAmplitude = peak
		}
		rep.WindowCount++
		winStart := float64(start) / float64(opt.SampleRate)
		if !open && rms <= enterAmp {
			open = true
			openStart = winStart
		} else if open && rms >= exitAmp {
			closeRegion(&rep, openStart, winStart, opt)
			open = false
			openStart = -1
		}
	}
	duration := float64(total) / float64(opt.SampleRate)
	if open {
		closeRegion(&rep, openStart, duration, opt)
	}
	if rep.PeakAmplitude > 0 {
		rep.PeakDBFS = amplitudeToDB(rep.PeakAmplitude)
	} else {
		rep.PeakDBFS = math.Inf(-1)
	}
	markBoundaries(rep.Regions, duration)
	return rep, nil
}

func closeRegion(rep *Report, start, end float64, opt Options) {
	if end < start {
		end = start
	}
	d := end - start
	if d*1000 < float64(opt.MinSilenceMS) {
		rep.ShortSuppressed++
		return
	}
	rep.Regions = append(rep.Regions, Region{StartSec: roundMS(start), EndSec: roundMS(end), DurationSec: roundMS(d)})
}

func markBoundaries(rs []Region, duration float64) {
	const eps = 0.025
	for i := range rs {
		atStart := rs[i].StartSec <= eps
		atEnd := duration-rs[i].EndSec <= eps
		switch {
		case atStart && atEnd:
			rs[i].Boundary = "both"
		case atStart:
			rs[i].Boundary = "start"
		case atEnd:
			rs[i].Boundary = "end"
		}
	}
}

// PCM16Mono converts little-endian signed 16-bit PCM bytes to float samples.
// For multichannel PCM, channels are averaged frame-by-frame.
func PCM16ToFloat(b []byte, channels int) []float64 {
	if channels <= 0 {
		channels = 1
	}
	frames := len(b) / 2 / channels
	out := make([]float64, frames)
	for f := 0; f < frames; f++ {
		sum := 0.0
		for c := 0; c < channels; c++ {
			i := (f*channels + c) * 2
			if i+1 >= len(b) {
				break
			}
			v := int16(uint16(b[i]) | uint16(b[i+1])<<8)
			sum += float64(v) / 32768.0
		}
		out[f] = sum / float64(channels)
	}
	return out
}

func dbToAmplitude(db float64) float64 { return math.Pow(10, db/20) }
func amplitudeToDB(a float64) float64  { return 20 * math.Log10(a) }
func roundMS(v float64) float64        { return math.Round(v*1000) / 1000 }
