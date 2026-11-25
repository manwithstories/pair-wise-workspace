package fixture

import (
	"encoding/binary"
	"math"
)

// generateSegmentPCM creates deterministic 44.1 kHz, 6-channel PCM16LE bytes.
// The global sample index keeps phases continuous across segment boundaries.
func generateSegmentPCM(seed int64, segment int) []byte {
	out := make([]byte, AudioFramesPerSeg*SourceChannels*2)
	freqs := [SourceChannels]float64{220, 277.18, 329.63, 73.42, 440, 554.37}
	amps := [SourceChannels]float64{5200, 4300, 3600, 5000, 3000, 2600}
	seedPhase := float64(seed) * 0.0137
	globalStart := segment * AudioFramesPerSeg
	for f := 0; f < AudioFramesPerSeg; f++ {
		global := globalStart + f
		for ch := 0; ch < SourceChannels; ch++ {
			phase := 2 * math.Pi * freqs[ch] * float64(global) / SourceRate
			// A small deterministic amplitude modulation makes LUFS metering real.
			mod := 0.92 + 0.08*math.Sin(2*math.Pi*3.5*float64(global)/SourceRate+seedPhase)
			seedBend := 1 + 0.01*math.Sin(float64(global)*0.0007+float64(seed))
			v := amps[ch] * mod * seedBend * math.Sin(phase+seedPhase*float64(ch+1))
			if v > 32767 {
				v = 32767
			} else if v < -32768 {
				v = -32768
			}
			off := (f*SourceChannels + ch) * 2
			binary.LittleEndian.PutUint16(out[off:off+2], uint16(int16(math.Round(v))))
		}
	}
	return out
}

// ExpectedKeyframeIndices returns global NAL indices for frames divisible by three.
func ExpectedKeyframeIndices() []int {
	total := SegmentCount * FramesPerSegment
	var out []int
	for frame := 0; frame < total; frame++ {
		if frame%3 == 0 {
			out = append(out, frame)
		}
	}
	return out
}
