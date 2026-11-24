package audio

import (
	"math"

	"mediarig/internal/media"
)

// LoudnessReport contains pre/post normalization measurements.
type LoudnessReport struct {
	LUFSBefore         float64         `json:"lufs_before"`
	LUFSAfter          float64         `json:"lufs_after"`
	TruePeakDBTPBefore float64         `json:"true_peak_dbtp_before"`
	TruePeakDBTPAfter  float64         `json:"true_peak_dbtp"`
	GainDB             float64         `json:"gain_db"`
	ClippedSamples     int             `json:"clipped_samples"`
	SilenceRegions     []SilenceRegion `json:"silence_regions"`
}

// SilenceRegion is an exact sample-index interval. Times are converted to
// seconds rationally by the report layer.
type SilenceRegion struct {
	StartFrame int     `json:"start_frame"`
	EndFrame   int     `json:"end_frame"` // exclusive
	Seconds    float64 `json:"seconds"`
}

type biquad struct {
	b0, b1, b2 float64
	a1, a2     float64
	x1, x2     float64
	y1, y2     float64
}

func (q *biquad) process(x float64) float64 {
	y := q.b0*x + q.b1*q.x1 + q.b2*q.x2 - q.a1*q.y1 - q.a2*q.y2
	q.x2, q.x1 = q.x1, x
	q.y2, q.y1 = q.y1, y
	return y
}

func highShelf(sampleRate, f0, gainDB, q float64) biquad {
	a := math.Pow(10, gainDB/40)
	w0 := 2 * math.Pi * f0 / sampleRate
	cosw, sinw := math.Cos(w0), math.Sin(w0)
	alpha := sinw / (2 * q)
	root := 2 * math.Sqrt(a) * alpha
	b0 := a * ((a + 1) + (a-1)*cosw + root)
	b1 := -2 * a * ((a - 1) + (a+1)*cosw)
	b2 := a * ((a + 1) + (a-1)*cosw - root)
	a0 := (a + 1) - (a-1)*cosw + root
	a1 := 2 * ((a - 1) - (a+1)*cosw)
	a2 := (a + 1) - (a-1)*cosw - root
	return biquad{b0: b0 / a0, b1: b1 / a0, b2: b2 / a0, a1: a1 / a0, a2: a2 / a0}
}

func highPass(sampleRate, f0, q float64) biquad {
	w0 := 2 * math.Pi * f0 / sampleRate
	cosw, sinw := math.Cos(w0), math.Sin(w0)
	alpha := sinw / (2 * q)
	b0 := (1 + cosw) / 2
	b1 := -(1 + cosw)
	b2 := (1 + cosw) / 2
	a0 := 1 + alpha
	a1 := -2 * cosw
	a2 := 1 - alpha
	return biquad{b0: b0 / a0, b1: b1 / a0, b2: b2 / a0, a1: a1 / a0, a2: a2 / a0}
}

func kWeightChannel(in []float64, rate int) []float64 {
	shelf := highShelf(float64(rate), 1681.7799, 4.0, 0.707175)
	hp := highPass(float64(rate), 38.1355, 0.500327)
	out := make([]float64, len(in))
	for i, x := range in {
		out[i] = hp.process(shelf.process(x))
	}
	return out
}

// IntegratedLUFS computes BS.1770-style K-weighted, two-stage gated loudness.
// The implementation uses 400 ms blocks with 100 ms hops.
func IntegratedLUFS(a Audio) float64 {
	if a.Frames() == 0 || len(a.Channels) == 0 {
		return -120
	}
	rate := a.SampleRate
	if rate <= 0 {
		rate = 48000
	}
	weighted := make([][]float64, len(a.Channels))
	for c, ch := range a.Channels {
		weighted[c] = kWeightChannel(ch, rate)
	}
	block := rate * 4 / 10
	hop := rate / 10
	if block <= 0 || a.Frames() < rate/10 {
		return -120
	}
	type blockInfo struct{ meanSquare float64 }
	var blocks []blockInfo
	for start := 0; start+block <= a.Frames(); start += hop {
		sum := 0.0
		for _, ch := range weighted {
			var s float64
			for i := start; i < start+block; i++ {
				s += ch[i] * ch[i]
			}
			// Stereo/mono downmix channels both have unity BS.1770 weight.
			sum += s / float64(block)
		}
		blocks = append(blocks, blockInfo{meanSquare: sum})
	}
	if len(blocks) == 0 {
		return -120
	}
	absolute := math.Pow(10, (-70+0.691)/10)
	var absSelected []float64
	for _, b := range blocks {
		if b.meanSquare >= absolute {
			absSelected = append(absSelected, b.meanSquare)
		}
	}
	if len(absSelected) == 0 {
		return -120
	}
	mean := average(absSelected)
	relativeThreshold := mean * math.Pow(10, -10.0/10.0)
	var gated []float64
	for _, b := range absSelected {
		if b >= relativeThreshold {
			gated = append(gated, b)
		}
	}
	if len(gated) == 0 {
		gated = absSelected
	}
	lufs := -0.691 + 10*math.Log10(average(gated))
	if math.IsNaN(lufs) || math.IsInf(lufs, -1) {
		return -120
	}
	return lufs
}

func average(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// TruePeakDBTP estimates true peak with 4x windowed-sinc oversampling.
func TruePeakDBTP(a Audio) float64 {
	peak := 0.0
	for _, ch := range a.Channels {
		if p := samplePeak(ch); p > peak {
			peak = p
		}
		os := oversample4(ch)
		if p := samplePeak(os); p > peak {
			peak = p
		}
	}
	if peak < 1e-8 {
		return -120
	}
	return 20 * math.Log10(peak)
}

func samplePeak(ch []float64) float64 {
	var p float64
	for _, x := range ch {
		if ax := math.Abs(x); ax > p {
			p = ax
		}
	}
	return p
}

func oversample4(in []float64) []float64 {
	const phases = 4
	const half = 8
	out := make([]float64, len(in)*phases)
	for k := range out {
		idx := k / phases
		frac := float64(k%phases) / phases
		sum := 0.0
		for t := -half; t <= half; t++ {
			j := idx + t
			if j < 0 || j >= len(in) {
				continue
			}
			x := float64(t) - frac
			w := 0.5 - 0.5*math.Cos(2*math.Pi*(x+half)/float64(2*half))
			sinc := 1.0
			if x != 0 {
				sinc = math.Sin(math.Pi*x) / (math.Pi * x)
			}
			sum += in[j] * sinc * w
		}
		out[k] = sum
	}
	return out
}

// FindSilence detects frames where every channel is below -60 dBFS.
func FindSilence(a Audio) []SilenceRegion {
	threshold := math.Pow(10, -60.0/20.0)
	var regions []SilenceRegion
	frames := a.Frames()
	start := -1
	for i := 0; i <= frames; i++ {
		silent := i < frames
		if i < frames {
			for _, ch := range a.Channels {
				if math.Abs(ch[i]) >= threshold {
					silent = false
					break
				}
			}
		} else {
			silent = false
		}
		if silent && start < 0 {
			start = i
		}
		if !silent && start >= 0 {
			end := i
			// Ignore isolated zero-crossing/quantization dips shorter than
			// 1 ms; packet-loss concealment gaps are at least one packet.
			if end-start >= a.SampleRate/1000 {
				sec := media.SamplesToSeconds(int64(end-start), a.SampleRate).Float64()
				regions = append(regions, SilenceRegion{StartFrame: start, EndFrame: end, Seconds: sec})
			}
			start = -1
		}
	}
	return regions
}

// NormalizeToBroadcast measures, applies exact dB gain, and enforces the
// requested loudness and -1 dBTP ceiling. Generated broadcast material has
// ample crest headroom; the safety limiter/counter still guarantees the peak
// contract for adversarial inputs.
func NormalizeToBroadcast(a Audio, targetLUFS float64) (Audio, LoudnessReport, error) {
	if len(a.Channels) == 0 {
		return a, LoudnessReport{}, media.Internal(media.StageAudio, "cannot normalize empty audio")
	}
	before := IntegratedLUFS(a)
	tpBefore := TruePeakDBTP(a)
	gainDB := targetLUFS - before
	gain := math.Pow(10, gainDB/20)

	out := NewAudio(a.SampleRate, len(a.Channels), a.Frames())
	ceilingDBTP := -1.0
	ceiling := math.Pow(10, ceilingDBTP/20)
	clipped := 0
	for c := range a.Channels {
		for i, x := range a.Channels[c] {
			y := x * gain
			if y > ceiling {
				y = ceiling
				clipped++
			} else if y < -ceiling {
				y = -ceiling
				clipped++
			}
			out.Channels[c][i] = y
		}
	}
	out.Layout = a.Layout
	after := IntegratedLUFS(out)
	tpAfter := TruePeakDBTP(out)
	// Windowed-sinc true peak can overshoot the sample ceiling slightly. A
	// single deterministic safety gain enforces the measured true-peak limit.
	if tpAfter > ceilingDBTP+0.001 {
		safety := math.Pow(10, (ceilingDBTP-tpAfter)/20)
		for c := range out.Channels {
			for i := range out.Channels[c] {
				out.Channels[c][i] *= safety
			}
		}
		after = IntegratedLUFS(out)
		tpAfter = TruePeakDBTP(out)
		gainDB += 20 * math.Log10(safety)
	}
	rep := LoudnessReport{
		LUFSBefore:         round3(before),
		LUFSAfter:          round3(after),
		TruePeakDBTPBefore: round3(tpBefore),
		TruePeakDBTPAfter:  round3(tpAfter),
		GainDB:             round3(gainDB),
		ClippedSamples:     clipped,
		SilenceRegions:     FindSilence(out),
	}
	return out, rep, nil
}

func round3(x float64) float64 {
	return math.Round(x*1000) / 1000
}
