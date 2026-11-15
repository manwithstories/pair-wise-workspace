package dsp

import "math"

type Coeffs struct {
	B0, B1, B2, A1, A2 float64
}

type Biquad struct {
	Coeffs
	z1, z2 float64
}

func NewBiquad(c Coeffs) *Biquad {
	return &Biquad{Coeffs: c}
}

func (b *Biquad) Reset() {
	b.z1, b.z2 = 0, 0
}

func (b *Biquad) Process(x float64) float64 {
	y := b.B0*x + b.z1
	b.z1 = b.B1*x - b.A1*y + b.z2
	b.z2 = b.B2*x - b.A2*y
	return y
}

func (b *Biquad) ProcessBuffer(in []float64) []float64 {
	out := make([]float64, len(in))
	for i, value := range in {
		out[i] = b.Process(value)
	}
	return out
}

// RB cookbook high shelf, used as the BS.1770 pre-filter stage.
// BS.1770 stage 1 uses gain 4 dB, f0 1500 Hz, Q 1/sqrt(2).
func HighShelf(sampleRate, gainDB, cutoffHz float64) Coeffs {
	a := math.Pow(10, gainDB/40)
	omega := 2 * math.Pi * cutoffHz / sampleRate
	cosW := math.Cos(omega)
	sinW := math.Sin(omega)
	alpha := sinW / (2 * math.Sqrt(0.5))
	sqrtA := 2 * math.Sqrt(a) * alpha
	coef := Coeffs{}
	b0 := a * ((a + 1) + (a-1)*cosW + sqrtA)
	b1 := -2 * a * ((a - 1) + (a+1)*cosW)
	b2 := a * ((a + 1) + (a-1)*cosW - sqrtA)
	a0 := (a + 1) - (a-1)*cosW + sqrtA
	a1 := 2 * ((a - 1) - (a+1)*cosW)
	a2 := (a + 1) - (a-1)*cosW - sqrtA
	coef.B0, coef.B1, coef.B2 = b0/a0, b1/a0, b2/a0
	coef.A1, coef.A2 = a1/a0, a2/a0
	return coef
}

// RB cookbook high-pass, used as the BS.1770 RLB weighting stage.
func HighPass(sampleRate, cutoffHz, q float64) Coeffs {
	omega := 2 * math.Pi * cutoffHz / sampleRate
	cosW := math.Cos(omega)
	sinW := math.Sin(omega)
	alpha := sinW / (2 * q)
	coef := Coeffs{}
	a0 := 1 + alpha
	b0 := a0 * (1 + cosW) / 2
	b1 := -a0 * (1 + cosW)
	b2 := a0 * (1 + cosW) / 2
	a1 := -2 * cosW
	a2 := 1 - alpha
	coef.B0, coef.B1, coef.B2 = b0/a0, b1/a0, b2/a0
	coef.A1, coef.A2 = a1/a0, a2/a0
	return coef
}

// Digital transfer-function magnitude at normalized frequency f/fs.
func (c Coeffs) Magnitude(normalizedFreq float64) float64 {
	omega := 2 * math.Pi * normalizedFreq
	cosW, sinW := math.Cos(omega), math.Sin(omega)
	numRe := c.B0 + c.B1*cosW + c.B2*math.Cos(2*omega)
	numIm := -(c.B1*sinW + c.B2*math.Sin(2*omega))
	denRe := 1 + c.A1*cosW + c.A2*math.Cos(2*omega)
	denIm := -(c.A1*sinW + c.A2*math.Sin(2*omega))
	return math.Hypot(numRe, numIm) / math.Hypot(denRe, denIm)
}

func (c Coeffs) Stable() bool {
	if math.Abs(c.A2) >= 1 {
		return false
	}
	if math.Abs(c.A1) >= 1+c.A2+1e-12 {
		return false
	}
	return true
}

func MaxCoeffDelta(a, b Coeffs) float64 {
	return math.Max(
		math.Max(math.Abs(a.B0-b.B0), math.Max(math.Abs(a.B1-b.B1), math.Abs(a.B2-b.B2))),
		math.Max(math.Abs(a.A1-b.A1), math.Abs(a.A2-b.A2)),
	)
}
