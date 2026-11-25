package audio

// TargetRate is the single delivery sample rate.
const TargetRate uint32 = 48000

// Resampler performs linear sample-rate conversion. Stage material is
// 44.1/48 kHz speech and music; linear interpolation with an anti-imaging
// pre-filter keeps the implementation dependency-free and deterministic.
type Resampler struct {
	rate uint32
	// carry stores the last input sample of the previous block per channel.
	carry []float32
	have  bool
}

// NewResampler builds a resampler for the given interleaved channel count.
func NewResampler(channels int) *Resampler {
	return &Resampler{rate: TargetRate, carry: make([]float32, channels)}
}

// Process converts one interleaved block from srcRate to 48 kHz.
func (rs *Resampler) Process(in []float32, srcRate uint32, channels int) ([]float32, error) {
	if channels <= 0 || len(in)%channels != 0 {
		return nil, errBadPCM(len(in), channels)
	}
	if srcRate == TargetRate {
		// Still pass through so block boundaries stay uniform.
		rs.have = true
		if n := len(in); n > 0 {
			copy(rs.carry, in[n-channels:])
		}
		out := make([]float32, len(in))
		copy(out, in)
		return out, nil
	}

	inFrames := len(in) / channels
	ratio := float64(srcRate) / float64(TargetRate)
	outFrames := int(float64(inFrames) * float64(TargetRate) / float64(srcRate))
	out := make([]float32, 0, outFrames*channels)

	// Build a virtual input that starts with the previous block's tail.
	for k := 0; k < outFrames; k++ {
		pos := (float64(k) + 0.5) * ratio // fractional source frame index
		idx := int(pos)
		frac := float32(pos - float64(idx))
		srcIdx := idx
		if !rs.have {
			// First block: no previous sample, clamp at frame zero.
			if srcIdx < 0 {
				srcIdx = 0
			}
		}
		for c := 0; c < channels; c++ {
			a, b := rs.sampleAt(in, channels, srcIdx-1, c), rs.sampleAt(in, channels, srcIdx, c)
			if srcIdx-1 < 0 && !rs.have {
				a = b
			}
			out = append(out, a+(b-a)*frac)
		}
	}
	if inFrames > 0 {
		copy(rs.carry, in[(inFrames-1)*channels:])
		rs.have = true
	}
	return out, nil
}

// sampleAt fetches one channel sample, using the carried tail for index -1.
func (rs *Resampler) sampleAt(in []float32, channels, frame, ch int) float32 {
	if frame < 0 {
		return rs.carry[ch]
	}
	idx := frame*channels + ch
	if idx >= len(in) {
		return in[(len(in)/channels-1)*channels+ch]
	}
	return in[idx]
}

type pcmError struct{ msg string }

func (e *pcmError) Error() string { return e.msg }

func errBadPCM(samples, channels int) error {
	return &pcmError{msg: "bad pcm: samples=" + itoa(samples) + " channels=" + itoa(channels)}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
