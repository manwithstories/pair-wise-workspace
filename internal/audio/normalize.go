// Package audio normalizes recovered audio to a uniform delivery
// spec: 48 kHz stereo at -18 LUFS integrated loudness with true peak
// not exceeding -1 dBTP. Missing frames are filled with silence and
// their intervals preserved for the archive record.
package audio

import "math"

const (
	InRate  = 44100
	OutRate = 48000

	TargetLUFS      = -18.0
	MaxTruePeakDBTP = -1.0
)

// Result is the normalized audio plus the measurements a reviewer
// needs to verify the run.
type Result struct {
	Samples        []float32 // interleaved stereo at OutRate
	IntegratedLUFS float64
	TruePeakDBTP   float64
	Missing        [][2]int // output-sample ranges filled with silence
}

// Normalize resamples 44.1 kHz mono to 48 kHz stereo, splices silence
// into the missing input ranges, and normalizes loudness.
func Normalize(mono []float32, missing [][2]int) *Result {
	in := append([]float32(nil), mono...)
	for _, iv := range missing {
		for i := iv[0]; i < iv[1] && i < len(in); i++ {
			in[i] = 0
		}
	}
	out := resample(in)
	loud := integratedLUFS(out)
	gain := 1.0
	if loud > -70 { // silence is left untouched
		gain = math.Pow(10, (TargetLUFS-loud)/20)
	}
	applyGain(out, gain)
	if tp := truePeakDBTP(out); tp > MaxTruePeakDBTP {
		applyGain(out, math.Pow(10, (MaxTruePeakDBTP-tp)/20))
	}
	return &Result{
		Samples:        out,
		IntegratedLUFS: integratedLUFS(out),
		TruePeakDBTP:   truePeakDBTP(out),
		Missing:        mapMissing(missing, len(in)),
	}
}

// resample converts 44.1 kHz mono to interleaved 48 kHz stereo using
// exact 160/147 rational positioning and linear interpolation.
func resample(in []float32) []float32 {
	if len(in) == 0 {
		return nil
	}
	outLen := len(in) * 160 / 147
	out := make([]float32, outLen*2)
	for i := 0; i < outLen; i++ {
		pos := float64(i) * 147 / 160
		j := int(pos)
		frac := float32(pos - float64(j))
		var s float32
		if j+1 < len(in) {
			s = in[j]*(1-frac) + in[j+1]*frac
		} else {
			s = in[len(in)-1]
		}
		out[2*i], out[2*i+1] = s, s
	}
	return out
}

// mapMissing converts input-sample ranges to output-sample ranges.
func mapMissing(missing [][2]int, inLen int) [][2]int {
	var out [][2]int
	for _, iv := range missing {
		end := iv[1]
		if end > inLen {
			end = inLen
		}
		if iv[0] >= end {
			continue
		}
		out = append(out, [2]int{iv[0] * 160 / 147, end * 160 / 147})
	}
	return out
}

// biquad applies one second-order filter section in place.
func biquad(x []float64, b, a [3]float64) {
	var x1, x2, y1, y2 float64
	for i, v := range x {
		y := b[0]*v + b[1]*x1 + b[2]*x2 - a[1]*y1 - a[2]*y2
		x2, x1 = x1, v
		y2, y1 = y1, y
		x[i] = y
	}
}

// BS.1770 K-weighting coefficients for 48 kHz.
var (
	shelfB = [3]float64{1.53512485958697, -2.69169618940638, 1.19839281085285}
	shelfA = [3]float64{1, -1.69065929318241, 0.73248077421585}
	hpB    = [3]float64{1, -2, 1}
	hpA    = [3]float64{1, -1.99004745483398, 0.99007225036621}
)

// integratedLUFS measures K-weighted integrated loudness of
// interleaved stereo (both channels weighted 1.0).
func integratedLUFS(interleaved []float32) float64 {
	if len(interleaved) < 2 {
		return -70
	}
	n := len(interleaved) / 2
	ch := make([][]float64, 2)
	for c := 0; c < 2; c++ {
		ch[c] = make([]float64, n)
		for i := 0; i < n; i++ {
			ch[c][i] = float64(interleaved[2*i+c])
		}
		biquad(ch[c], shelfB, shelfA)
		biquad(ch[c], hpB, hpA)
	}
	var sum float64
	for c := 0; c < 2; c++ {
		var ms float64
		for _, v := range ch[c] {
			ms += v * v
		}
		sum += ms / float64(n)
	}
	if sum <= 0 {
		return -70
	}
	return -0.691 + 10*math.Log10(sum)
}

// truePeakDBTP estimates the true peak with 4x linear oversampling.
func truePeakDBTP(interleaved []float32) float64 {
	if len(interleaved) == 0 {
		return -120
	}
	peak := 0.0
	for c := 0; c < 2; c++ {
		prev := float64(interleaved[c])
		for i := 1; i < len(interleaved)/2; i++ {
			cur := float64(interleaved[2*i+c])
			for k := 0; k < 4; k++ {
				v := prev + (cur-prev)*float64(k)/4
				if v < 0 {
					v = -v
				}
				if v > peak {
					peak = v
				}
			}
			prev = cur
		}
	}
	if peak == 0 {
		return -120
	}
	return 20 * math.Log10(peak)
}

func applyGain(interleaved []float32, g float64) {
	if g == 1 {
		return
	}
	for i, v := range interleaved {
		interleaved[i] = float32(float64(v) * g)
	}
}
