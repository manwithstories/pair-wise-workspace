package audio

// SourceRates is the set of input sample rates exercised by fixtures.
var SourceRates = []int{8000, 16000, 22050, 32000, 44100, 48000, 96000}

// TonePCM builds a deterministic moderate-level triangle tone. The phase is an
// exact integer accumulator so output is stable across runs and architectures.
// frames at rate are produced; channels 1 or 2 use a per-channel phase offset.
func TonePCM(rate, channels, frames int, seed int64) *PCM {
	if channels <= 0 {
		channels = 1
	}
	// Frequency varies with seed but stays musical and bounded.
	freq := 220 + (seed%12)*40 // 220..660 Hz
	amp := int64(9000)
	// Phase increment per frame in mod-2^32 phase space: freq/rate * 2^32.
	step := uint32(uint64(freq) << 32 / uint64(rate))
	samples := make([]int16, frames*channels)
	lfsr := uint32(seed*2654435761 + 1)
	for f := 0; f < frames; f++ {
		ph := uint32(f) * step // wraps mod 2^32 deterministically
		lfsr ^= lfsr << 13
		lfsr ^= lfsr >> 17
		lfsr ^= lfsr << 5
		dither := int64(lfsr%65) - 32
		for c := 0; c < channels; c++ {
			chPhase := ph + uint32(int64(c)*int64(step)/2)
			v := triangle(chPhase)*amp/32767 + dither
			if v > 12000 {
				v = 12000
			}
			if v < -12000 {
				v = -12000
			}
			samples[f*channels+c] = int16(v)
		}
	}
	return &PCM{Rate: rate, Channels: channels, Samples: samples}
}

// triangle maps a phase accumulator to [-32767,32767] deterministically.
func triangle(phase uint32) int64 {
	const half = uint32(1) << 31
	x := phase
	if x < half {
		// rising: 0 -> max
		return int64(uint64(x) * 32767 / uint64(half))
	}
	// falling: max -> 0
	return int64(uint64(0xffffffff-x) * 32767 / uint64(half))
}

// ClippedPCM returns a buffer containing a run of full-scale samples.
func ClippedPCM(channels, totalFrames, clipStart, clipRun int) *PCM {
	p := TonePCM(48000, channels, totalFrames, 7)
	for f := clipStart; f < clipStart+clipRun && f < totalFrames; f++ {
		for c := 0; c < channels; c++ {
			if (f+c)%2 == 0 {
				p.Samples[f*channels+c] = 32767
			} else {
				p.Samples[f*channels+c] = -32768
			}
		}
	}
	return p
}
