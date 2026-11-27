package audio

import (
	"fmt"
	"math"
)

// TargetLUFS is the loudness target of the media core.
const TargetLUFS = -23.0

// TruePeakCeiling is the sample-peak ceiling used after gain is applied.
const TruePeakCeiling = 0.98 // about -0.18 dBFS

// KWeighting holds the two biquad stages of ITU-R BS.1770 K-weighting,
// designed for a fixed 48 kHz sample rate.
type KWeighting struct {
	shelf biquad
	high  biquad
}

type biquad struct {
	b0, b1, b2, a1, a2 float64
	z1, z2             float64
}

func (b *biquad) reset() { b.z1, b.z2 = 0, 0 }

func (b *biquad) process(x float64) float64 {
	y := b.b0*x + b.z1
	b.z1 = b.b1*x - b.a1*y + b.z2
	b.z2 = b.b2*x - b.a2*y
	return y
}

// NewKWeighting builds the BS.1770 filter pair for 48 kHz.
//
// Coefficients are the reference values from ITU-R BS.1770-4 Annex 1
// (pre-filter high shelf + RLB high pass); they are exact for 48 kHz, which is
// why the pipeline normalizes the rate before measuring loudness.
func NewKWeighting() *KWeighting {
	return &KWeighting{
		shelf: biquad{
			b0: 1.53512485958697, b1: -2.69169618940638, b2: 1.19839281085285,
			a1: -1.69065929318241, a2: 0.73248077421585,
		},
		high: biquad{
			b0: 1.0, b1: -2.0, b2: 1.0,
			a1: -1.99004745483398, a2: 0.99007225036621,
		},
	}
}

// Loudness is the result of a BS.1770 measurement.
type Loudness struct {
	IntegratedLUFS float64
	MaxMomentary   float64
	MaxShortTerm   float64
	SamplePeak     float64
	// GatedBlocks counts 400 ms blocks above the absolute gate.
	GatedBlocks int
	TotalBlocks int
	// Valid is false for silence or too-short input.
	Valid bool
}

// Meter measures integrated loudness with 400 ms blocks, 75% overlap and the
// -70 LUFS absolute gate, matching the ebur128 gate used by modern encoders.
// Each channel keeps its own filter state: sharing one would mix channels into
// a single signal and make the measurement depend on channel order.
type Meter struct {
	kws     []*KWeighting
	buf     [][]float64
	block   int
	hop     int
	blocks  []float64 // per-block mean square
	samples int64
	peak    float64
	maxM    float64
}

// NewMeter creates a meter for a 48 kHz stereo buffer.
func NewMeter(channels int) *Meter {
	block := TargetRate * 4 / 10 // 400 ms
	m := &Meter{
		buf:   make([][]float64, channels),
		block: block,
		hop:   block / 4,
	}
	for range m.buf {
		m.kws = append(m.kws, NewKWeighting())
	}
	return m
}

// Add consumes one planar buffer at 48 kHz.
func (m *Meter) Add(in Buffer) error {
	if in.Rate != TargetRate {
		return fmt.Errorf("audio: loudness requires %d Hz, got %d", TargetRate, in.Rate)
	}
	if err := in.Validate(); err != nil {
		return err
	}
	if len(in.Planes) != len(m.buf) {
		return fmt.Errorf("audio: loudness channel count changed")
	}
	for c := range in.Planes {
		for _, v := range in.Planes[c] {
			a := math.Abs(float64(v))
			if a > m.peak {
				m.peak = a
			}
		}
	}
	// K-weight and accumulate blocks with 75% overlap.
	for c := range in.Planes {
		kw := m.kws[c]
		for _, v := range in.Planes[c] {
			x := kw.shelf.process(float64(v))
			x = kw.high.process(x)
			m.buf[c] = append(m.buf[c], x)
		}
	}
	n := len(in.Planes[0])
	m.samples += int64(n)
	for len(m.buf[0]) >= m.block {
		var sum float64
		for c := range m.buf {
			var chSum float64
			for i := 0; i < m.block; i++ {
				v := m.buf[c][i]
				chSum += v * v
			}
			sum += chSum
		}
		// Stereo channel weights are both 1.0; mean square is per sample.
		meanSquare := sum / float64(m.block)
		m.blocks = append(m.blocks, meanSquare)
		if v := m.momentary(meanSquare); v > m.maxM {
			m.maxM = v
		}
		for c := range m.buf {
			m.buf[c] = append(m.buf[c][:0], m.buf[c][m.hop:]...)
		}
	}
	return nil
}

func (m *Meter) momentary(meanSquare float64) float64 {
	v := -0.691 + 10*math.Log10(meanSquare+1e-20)
	if math.Inf(-1) == v {
		return -math.MaxFloat64
	}
	return v
}

// Result finalizes the measurement.
func (m *Meter) Result() Loudness {
	l := Loudness{SamplePeak: m.peak, MaxMomentary: m.maxM, TotalBlocks: len(m.blocks)}
	if len(m.blocks) == 0 {
		return l
	}
	// Absolute gate then relative gate at -10 LU below the ungated mean.
	var sum float64
	var count int
	for _, ms := range m.blocks {
		if m.momentary(ms) > -70 {
			sum += ms
			count++
		}
	}
	if count == 0 {
		return l
	}
	rel := m.momentary(sum/float64(count)) - 10
	var gsum float64
	var gcount int
	for _, ms := range m.blocks {
		lu := m.momentary(ms)
		if lu > -70 && lu > rel {
			gsum += ms
			gcount++
		}
	}
	if gcount == 0 {
		return l
	}
	l.IntegratedLUFS = m.momentary(gsum / float64(gcount))
	l.MaxShortTerm = l.MaxMomentary // short-term not tracked separately
	l.GatedBlocks = gcount
	l.Valid = true
	return l
}

// ApplyGain scales every plane in place by gain.
func ApplyGain(b Buffer, gain float64) Buffer {
	out := Buffer{Rate: b.Rate, Layout: b.Layout, Planes: make([][]float32, len(b.Planes))}
	for c := range b.Planes {
		out.Planes[c] = make([]float32, len(b.Planes[c]))
		for i, v := range b.Planes[c] {
			out.Planes[c][i] = float32(float64(v) * gain)
		}
	}
	return out
}

// GainForTarget returns the linear gain that brings integrated loudness to
// target, clamped so the sample peak stays below TruePeakCeiling.
func GainForTarget(l Loudness, target float64) (gain float64, limited bool) {
	if !l.Valid {
		return 1, false
	}
	db := target - l.IntegratedLUFS
	g := math.Pow(10, db/20)
	if l.SamplePeak > 0 && l.SamplePeak*g > TruePeakCeiling {
		g = TruePeakCeiling / l.SamplePeak
		limited = true
	}
	return g, limited
}

// Peak returns the maximum absolute sample value.
func Peak(b Buffer) float64 {
	var p float64
	for _, plane := range b.Planes {
		for _, v := range plane {
			if a := math.Abs(float64(v)); a > p {
				p = a
			}
		}
	}
	return p
}

// RMSDBFS returns the RMS level of a buffer in dBFS.
func RMSDBFS(b Buffer) float64 {
	var sum float64
	var n int
	for _, plane := range b.Planes {
		for _, v := range plane {
			sum += float64(v) * float64(v)
			n++
		}
	}
	if n == 0 || sum == 0 {
		return math.Inf(-1)
	}
	return 10 * math.Log10(sum/float64(n))
}
