// Package audio normalises every track to 48 kHz stereo. It resamples with a
// windowed-sinc kernel, maps mono/multichannel/missing layouts through an
// explicit, auditable fallback policy, and applies sliding-window loudness
// normalisation toward -16 LUFS with a true-peak ceiling of -1 dBTP.
package audio

import (
	"fmt"
	"math"

	merr "mediaroute/internal/errors"
)

// Target output format.
const (
	TargetRate     = 48000
	TargetChannels = 2
	// TargetLUFS is the integrated loudness target.
	TargetLUFS = -16.0
	// MaxTruePeakDBTP bounds the true peak.
	MaxTruePeakDBTP = -1.0
)

// Layout names accepted by ResolveLayout.
const (
	LayoutMono    = "mono"
	LayoutStereo  = "stereo"
	Layout51      = "5.1"
	LayoutUnknown = "unknown"
	LayoutMissing = "missing"
)

// Config describes the source audio.
type Config struct {
	SampleRate int
	Channels   int
	// Layout is the declared layout or empty/"unknown" for a fallback.
	Layout string
}

// LayoutDecision records how the channel layout was resolved so operators can
// audit the fallback.
type LayoutDecision struct {
	Resolved string
	Fallback bool
	Reason   string
}

// ResolveLayout selects a channel mapping. A missing or unrecognised layout
// never fails silently: it falls back to downmix-to-stereo and is flagged.
func ResolveLayout(cfg Config) (LayoutDecision, error) {
	if cfg.SampleRate <= 0 || cfg.SampleRate > 384000 {
		return LayoutDecision{}, merr.New(merr.KindAudio, 0, -1, -1, false, "invalid sample rate %d", cfg.SampleRate)
	}
	if cfg.Channels <= 0 || cfg.Channels > 32 {
		return LayoutDecision{}, merr.New(merr.KindAudio, 0, -1, -1, false, "invalid channel count %d", cfg.Channels)
	}
	declared := cfg.Layout
	if declared == "" {
		declared = LayoutMissing
	}
	switch declared {
	case LayoutMono:
		if cfg.Channels != 1 {
			return LayoutDecision{}, merr.New(merr.KindAudio, 0, -1, -1, false,
				"layout mono contradicts %d channels", cfg.Channels)
		}
		return LayoutDecision{Resolved: LayoutMono, Reason: "declared mono"}, nil
	case LayoutStereo:
		if cfg.Channels != 2 {
			return LayoutDecision{}, merr.New(merr.KindAudio, 0, -1, -1, false,
				"layout stereo contradicts %d channels", cfg.Channels)
		}
		return LayoutDecision{Resolved: LayoutStereo, Reason: "declared stereo"}, nil
	case Layout51:
		if cfg.Channels != 6 {
			return LayoutDecision{}, merr.New(merr.KindAudio, 0, -1, -1, false,
				"layout 5.1 contradicts %d channels", cfg.Channels)
		}
		return LayoutDecision{Resolved: Layout51, Reason: "declared 5.1"}, nil
	case LayoutUnknown, LayoutMissing:
		return fallback(cfg, fmt.Sprintf("layout %q", declared))
	default:
		return fallback(cfg, fmt.Sprintf("unrecognised layout %q", declared))
	}
}

func fallback(cfg Config, why string) (LayoutDecision, error) {
	switch cfg.Channels {
	case 1:
		return LayoutDecision{Resolved: LayoutMono, Fallback: true, Reason: why + ": inferred mono from channel count"}, nil
	case 2:
		return LayoutDecision{Resolved: LayoutStereo, Fallback: true, Reason: why + ": inferred stereo from channel count"}, nil
	case 6:
		return LayoutDecision{Resolved: Layout51, Fallback: true, Reason: why + ": inferred 5.1 from channel count"}, nil
	default:
		return LayoutDecision{Resolved: LayoutUnknown, Fallback: true,
			Reason: fmt.Sprintf("%s: downmixing %d channels to stereo", why, cfg.Channels)}, nil
	}
}

// Convert resamples and maps source interleaved samples into 48 kHz stereo.
// The returned buffer is interleaved L,R. It is intentionally chunk-agnostic:
// callers may pass the whole track or windows.
func Convert(cfg Config, src []float32) ([]float32, LayoutDecision, error) {
	decision, err := ResolveLayout(cfg)
	if err != nil {
		return nil, decision, err
	}
	if cfg.Channels <= 0 || len(src)%cfg.Channels != 0 {
		return nil, decision, merr.New(merr.KindAudio, 0, -1, -1, false,
			"sample buffer length %d is not a multiple of %d channels", len(src), cfg.Channels)
	}
	frames := len(src) / cfg.Channels
	stereo := make([]float32, frames*TargetChannels)
	for i := 0; i < frames; i++ {
		base := i * cfg.Channels
		l, r := downmix(src[base:base+cfg.Channels], cfg.Channels)
		stereo[i*2] = l
		stereo[i*2+1] = r
	}
	if cfg.SampleRate != TargetRate {
		stereo = Resample(stereo, cfg.SampleRate, TargetRate)
	}
	return stereo, decision, nil
}

// downmix maps one source frame to stereo L/R with ITU-style coefficients.
func downmix(frame []float32, channels int) (float32, float32) {
	switch channels {
	case 1:
		return frame[0], frame[0]
	case 2:
		return frame[0], frame[1]
	case 6:
		// L R C LFE Ls Rs
		l := frame[0] + 0.7071*frame[2] + 0.7071*frame[4] + 0.5*frame[3]
		r := frame[1] + 0.7071*frame[2] + 0.7071*frame[5] + 0.5*frame[3]
		return clamp(l), clamp(r)
	default:
		// Generic: first two channels keep identity, the rest are averaged in.
		l, r := frame[0], frame[1%channels]
		var extra float32
		var n int
		for _, v := range frame[2:] {
			extra += v
			n++
		}
		if n > 0 {
			extra /= float32(n)
			l = clamp(l + 0.5*extra)
			r = clamp(r + 0.5*extra)
		}
		return l, r
	}
}

func clamp(v float32) float32 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

// Resample converts interleaved stereo from inRate to outRate using a
// windowed-sinc kernel with a 16-tap half-width and a Kaiser-like window.
// It preserves interleaving and returns the same channel count.
func Resample(in []float32, inRate, outRate int) []float32 {
	if inRate == outRate || inRate <= 0 || outRate <= 0 {
		return in
	}
	frames := len(in) / TargetChannels
	if frames == 0 {
		return []float32{}
	}
	outFrames := int(math.Round(float64(frames) * float64(outRate) / float64(inRate)))
	out := make([]float32, outFrames*TargetChannels)
	ratio := float64(inRate) / float64(outRate)
	const halfWidth = 8
	for i := 0; i < outFrames; i++ {
		srcPos := float64(i) * ratio
		center := int(math.Floor(srcPos))
		for ch := 0; ch < TargetChannels; ch++ {
			var acc, norm float64
			for t := center - halfWidth + 1; t <= center+halfWidth; t++ {
				if t < 0 || t >= frames {
					continue
				}
				x := srcPos - float64(t)
				w := sinc(x) * window(x, halfWidth)
				acc += float64(in[t*TargetChannels+ch]) * w
				norm += w
			}
			if norm != 0 {
				acc /= norm
			}
			out[i*TargetChannels+ch] = clamp(float32(acc))
		}
	}
	return out
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	px := math.Pi * x
	return math.Sin(px) / px
}

func window(x float64, half int) float64 {
	if math.Abs(x) >= float64(half) {
		return 0
	}
	// Blackman window on the normalised tap position.
	r := (x/float64(half) + 1) / 2
	return 0.42 - 0.5*math.Cos(2*math.Pi*r) + 0.08*math.Cos(4*math.Pi*r)
}

// MeasureLoudness returns the RMS level in dBFS for a block of stereo samples.
func MeasureLoudness(block []float32) float64 {
	if len(block) == 0 {
		return -math.Inf(1)
	}
	var sum float64
	for _, v := range block {
		sum += float64(v) * float64(v)
	}
	rms := math.Sqrt(sum / float64(len(block)))
	if rms <= 0 {
		return -math.Inf(1)
	}
	return 20 * math.Log10(rms)
}

// TruePeakDBTP estimates the inter-sample peak by 4x windowed-sinc
// oversampling of each channel, returning dBTP relative to full scale.
func TruePeakDBTP(samples []float32) float64 {
	frames := len(samples) / TargetChannels
	if frames == 0 {
		return -math.Inf(1)
	}
	peak := 0.0
	for ch := 0; ch < TargetChannels; ch++ {
		for i := 0; i < frames; i++ {
			a := math.Abs(float64(samples[i*TargetChannels+ch]))
			if a > peak {
				peak = a
			}
		}
		const half = 8
		const oversample = 4
		for i := 0; i < frames-1; i++ {
			for k := 1; k < oversample; k++ {
				pos := float64(i) + float64(k)/oversample
				center := int(math.Floor(pos))
				var acc, norm float64
				for t := center - half + 1; t <= center+half; t++ {
					if t < 0 || t >= frames {
						continue
					}
					x := pos - float64(t)
					w := sinc(x) * window(x, half)
					acc += float64(samples[t*TargetChannels+ch]) * w
					norm += w
				}
				if norm != 0 {
					if v := math.Abs(acc / norm); v > peak {
						peak = v
					}
				}
			}
		}
	}
	if peak <= 0 {
		return -math.Inf(1)
	}
	return 20 * math.Log10(peak)
}

// LoudnessResult records the normalisation outcome for verification.
type LoudnessResult struct {
	Samples []float32
	// WindowLUFS holds one measured level per sliding window of the input.
	WindowLUFS []float64
	// IntegratedLUFS is the gated input loudness before normalisation.
	IntegratedLUFS float64
	// OutputLUFS is the gated loudness measured after gain and limiting.
	OutputLUFS float64
	// TruePeakDBTP is the output true peak.
	TruePeakDBTP    float64
	GainDB          float64
	Limited         bool
	AppliedLoudness bool
}

// Normalize applies sliding-window loudness gating and a true-peak limiter.
// The window is 400 ms with 75% overlap (K-weighting is approximated by an
// RMS measure on the already band-limited demo material). Gain is capped so
// the result never exceeds MaxTruePeakDBTP.
func Normalize(samples []float32, windowMS int) LoudnessResult {
	res := LoudnessResult{Samples: samples}
	if len(samples) == 0 {
		return res
	}
	if windowMS <= 0 {
		windowMS = 400
	}
	windowFrames := TargetRate * windowMS / 1000
	hopFrames := windowFrames / 4
	if hopFrames < 1 {
		hopFrames = 1
	}
	frames := len(samples) / TargetChannels
	var blocks []float64
	for start := 0; start+windowFrames <= frames; start += hopFrames {
		l := MeasureLoudness(samples[start*TargetChannels : (start+windowFrames)*TargetChannels])
		if math.IsInf(l, -1) {
			continue
		}
		blocks = append(blocks, l)
	}
	if len(blocks) == 0 {
		return res
	}
	// Gate: ignore blocks more than 10 dB below the mean (approximate LUFS).
	var mean float64
	for _, b := range blocks {
		mean += b
	}
	mean /= float64(len(blocks))
	var gated []float64
	for _, b := range blocks {
		if b >= mean-10 {
			gated = append(gated, b)
		}
	}
	if len(gated) == 0 {
		gated = blocks
	}
	var integrated float64
	for _, b := range gated {
		integrated += math.Pow(10, b/10)
	}
	integrated = 10 * math.Log10(integrated/float64(len(gated)))
	res.WindowLUFS = blocks
	res.IntegratedLUFS = integrated
	res.AppliedLoudness = true

	gainDB := TargetLUFS - integrated
	// Limit gain so the true peak stays below the ceiling.
	peak := TruePeakDBTP(samples)
	if peak != math.Inf(-1) {
		maxGain := MaxTruePeakDBTP - peak
		if gainDB > maxGain {
			gainDB = maxGain
			res.Limited = true
		}
	}
	res.GainDB = gainDB
	gain := float32(math.Pow(10, gainDB/20))
	out := make([]float32, len(samples))
	for i, v := range samples {
		out[i] = v * gain
	}
	// Final true-peak limiter: apply a lookahead-free sample-domain limiter so
	// the oversampled estimate cannot exceed the ceiling either.
	for pass := 0; pass < 4; pass++ {
		outPeak := TruePeakDBTP(out)
		if outPeak <= MaxTruePeakDBTP {
			break
		}
		scale := float32(math.Pow(10, (MaxTruePeakDBTP-outPeak)/20))
		for i := range out {
			out[i] *= scale
		}
		res.Limited = true
	}
	res.Samples = out
	res.TruePeakDBTP = TruePeakDBTP(out)
	res.OutputLUFS = integratedLoudness(out, windowFrames, hopFrames)
	return res
}

// integratedLoudness measures gated loudness the same way Normalize does on
// its input, exposed separately so results can be independently verified.
func integratedLoudness(samples []float32, windowFrames, hopFrames int) float64 {
	frames := len(samples) / TargetChannels
	var blocks []float64
	for start := 0; start+windowFrames <= frames; start += hopFrames {
		if frames-start < windowFrames {
			break
		}
		l := MeasureLoudness(samples[start*TargetChannels : (start+windowFrames)*TargetChannels])
		if math.IsInf(l, -1) {
			continue
		}
		blocks = append(blocks, l)
	}
	if len(blocks) == 0 {
		return -math.Inf(1)
	}
	var mean float64
	for _, b := range blocks {
		mean += b
	}
	mean /= float64(len(blocks))
	var sum float64
	var n int
	for _, b := range blocks {
		if b >= mean-10 {
			sum += math.Pow(10, b/10)
			n++
		}
	}
	if n == 0 {
		sum, n = 0, len(blocks)
		for _, b := range blocks {
			sum += math.Pow(10, b/10)
		}
	}
	return 10 * math.Log10(sum/float64(n))
}
