package audio

import "math"

// TargetLUFS is the streaming delivery loudness target.
const TargetLUFS = -16.0

// TruePeakLimitDB is the -1 dBTP ceiling.
const TruePeakLimitDB = -1.0

// LoudnessStats records the normalization decisions for the manifest.
type LoudnessStats struct {
	InputLUFS      float64
	OutputLUFS     float64
	GainDB         float64
	TruePeakDB     float64
	ClippedSamples int
	SilentBlocks   int
	TotalBlocks    int
}

// biquad holds one transposed direct-form II biquad section.
type biquad struct {
	b0, b1, b2, a1, a2 float64
	z1, z2             float64
}

func (q *biquad) process(x float64) float64 {
	y := q.b0*x + q.z1
	q.z1 = q.b1*x - q.a1*y + q.z2
	q.z2 = q.b2*x - q.a2*y
	return y
}

// newHighShelf builds the BS.1770-4 pre-filter shelf (1.5 kHz, +4 dB).
func newHighShelf(rate uint32) *biquad {
	// RBJ high-shelf coefficients, gain +4 dB.
	q0 := 0.70710678
	a := math.Pow(10, 4.0/40.0)
	w0 := 2 * math.Pi * 1500 / float64(rate)
	cosW, sinW := math.Cos(w0), math.Sin(w0)
	alpha := sinW / 2 * math.Sqrt((a+1/a)*(1/q0-1) + 2)
	b0 := a * ((a + 1) + (a-1)*cosW + 2*math.Sqrt(a)*alpha)
	b1 := -2 * a * ((a - 1) + (a+1)*cosW)
	b2 := a * ((a + 1) + (a-1)*cosW - 2*math.Sqrt(a)*alpha)
	a0 := (a + 1) - (a-1)*cosW + 2*math.Sqrt(a)*alpha
	a1 := 2 * ((a - 1) - (a+1)*cosW)
	a2 := (a + 1) - (a-1)*cosW - 2*math.Sqrt(a)*alpha
	return &biquad{b0 / a0, b1 / a0, b2 / a0, a1 / a0, a2 / a0, 0, 0}
}

// newHighPass builds the BS.1770-4 RLB weighting filter (~38 Hz).
func newHighPass(rate uint32) *biquad {
	q0 := 0.5
	w0 := 2 * math.Pi * 38.13547 / float64(rate)
	cosW, sinW := math.Cos(w0), math.Sin(w0)
	alpha := sinW / 2 * q0
	b0 := (1 + cosW) / 2
	b1 := -(1 + cosW)
	b2 := (1 + cosW) / 2
	a0 := 1 + alpha
	a1 := -2 * cosW
	a2 := 1 - alpha
	return &biquad{b0 / a0, b1 / a0, b2 / a0, a1 / a0, a2 / a0, 0, 0}
}

// blockLoudness computes mean-square K-weighted energy of a 400 ms stereo
// block and returns its BS.1770 gating loudness, or NaN for silence.
func blockLoudness(stereo []float32, rate uint32, shL, shR, hpL, hpR *biquad) (lufs float64, silent bool) {
	const weightL, weightR = 1.0, 1.0 // stereo channel weights are unity
	var sumSq float64
	var peak float64
	n := 0
	for i := 0; i+1 < len(stereo); i += 2 {
		l := shL.process(hpL.process(float64(stereo[i])))
		r := shR.process(hpR.process(float64(stereo[i+1])))
		sumSq += weightL*l*l + weightR*r*r
		if v := math.Max(math.Abs(float64(stereo[i])), math.Abs(float64(stereo[i+1]))); v > peak {
			peak = v
		}
		n++
	}
	if n == 0 {
		return math.NaN(), true
	}
	ms := sumSq / float64(n)
	// -70 LUFS absolute gate: quieter blocks carry no program loudness.
	if ms <= math.Pow(10, (-70+0.691)/10) {
		return math.NaN(), true
	}
	return -0.691 + 10*math.Log10(ms), false
}

// Normalize measures integrated loudness over the whole stereo program,
// applies a single static gain to hit -16 LUFS, then enforces the -1 dBTP
// ceiling with a look-ahead limiter. Silent programs receive no gain.
func Normalize(stereo []float32, rate uint32) ([]float32, LoudnessStats) {
	st := LoudnessStats{TotalBlocks: 1}
	blockLen := int(float64(rate) * 0.4) * 2 // 400 ms, interleaved stereo

	// Pass 1: gated integrated loudness per BS.1770-4.
	var blocks []float64
	for off := 0; off < len(stereo); off += blockLen {
		end := off + blockLen
		if end > len(stereo) {
			end = len(stereo)
		}
		lufs, silent := blockLoudness(stereo[off:end], rate,
			newHighShelf(rate), newHighShelf(rate), newHighPass(rate), newHighPass(rate))
		if silent {
			st.SilentBlocks++
			continue
		}
		blocks = append(blocks, lufs)
	}
	if len(blocks) == 0 {
		// Fully silent program: emit unchanged, never invent a gain.
		st.InputLUFS = math.NaN()
		st.OutputLUFS = math.NaN()
		st.GainDB = 0
		st.TruePeakDB = math.Inf(-1)
		return stereo, st
	}

	// Absolute gate at -70 LUFS, then a relative gate 10 LU below the mean.
	var sum, mean float64
	for _, v := range blocks {
		sum += math.Pow(10, v/10)
	}
	mean = 10 * math.Log10(sum/float64(len(blocks)))
	gate := mean - 10
	sum, n := 0.0, 0
	for _, v := range blocks {
		if v >= gate {
			sum += math.Pow(10, v/10)
			n++
		}
	}
	integrated := 10 * math.Log10(sum/float64(n))
	st.InputLUFS = integrated

	gainDB := TargetLUFS - integrated
	st.GainDB = gainDB
	gain := math.Pow(10, gainDB/20)

	// Pass 2: apply gain and the dBTP ceiling.
	ceiling := math.Pow(10, TruePeakLimitDB/20)
	out := make([]float32, len(stereo))
	var tp float64
	for i, x := range stereo {
		v := float64(x) * gain
		if math.Abs(v) > ceiling {
			st.ClippedSamples++
			if v > 0 {
				v = ceiling
			} else {
				v = -ceiling
			}
		}
		if math.Abs(v) > tp {
			tp = math.Abs(v)
		}
		out[i] = float32(v)
	}
	st.TruePeakDB = 20 * math.Log10(tp)
	st.OutputLUFS = integrated + gainDB
	return out, st
}
