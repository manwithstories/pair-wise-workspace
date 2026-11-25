package fixture

import (
	"encoding/binary"
	"math"
)

// AudioParams fixes the private PCM audio track: 48 kHz, 16-bit little-endian
// mono, carried as stream type 0x06.
type AudioParams struct {
	SampleRate   int
	FrameSamples int // 960 == 20 ms at 48 kHz
}

// DefaultAudio is the 48 kHz / 20 ms configuration used by every fixture.
func DefaultAudio() AudioParams {
	return AudioParams{SampleRate: 48000, FrameSamples: 960}
}

// AudioTruth is the generated ground truth used to score silence detection.
type AudioTruth struct {
	// SilentFrame[i] is true when frame i was synthesized as silence.
	SilentFrame []bool
	FrameSamples int
}

// PCMFrame is one 20 ms PCM16LE block plus its monotonic frame index.
type PCMFrame struct {
	Index int
	PCM   []byte
}

// AudioSynth emits deterministic PCM: quiet or tone regions follow a fixed
// schedule so silence detection has known, frame-aligned ground truth.
type AudioSynth struct {
	p      AudioParams
	truth  AudioTruth
	frame  int
}

// NewAudioSynth prepares enough frames to cover duration90 ticks at 90 kHz.
func NewAudioSynth(p AudioParams, duration90 int64) *AudioSynth {
	ticksPerFrame := p.TicksPerFrame()
	nFrames := int((duration90 + ticksPerFrame - 1) / ticksPerFrame)
	s := &AudioSynth{p: p}
	s.truth = AudioTruth{
		SilentFrame:  make([]bool, nFrames),
		FrameSamples: p.FrameSamples,
	}
	return s
}

// Truth returns the frame-level silence ground truth.
func (s *AudioSynth) Truth() AudioTruth { return s.truth }

// IsSilentFrame encodes the deterministic silence schedule:
// frames 0..99 audible; 100..149 silence; 150..249 audible;
// 250..349 silence; then audible. Two long silent regions (>=1 s each)
// exercise hysteresis and merge logic.
func IsSilentFrame(frame int) bool {
	switch {
	case frame >= 100 && frame < 150:
		return true
	case frame >= 250 && frame < 350:
		return true
	default:
		return false
	}
}

// Next produces the next frame. Tone frames carry a low-amplitude 440 Hz
// sine (well above the silence floor); silent frames are near-zero with only
// ±1 LSB dither, so they never accidentally cross an energy threshold.
func (s *AudioSynth) Next() PCMFrame {
	idx := s.frame
	s.frame++
	if idx < len(s.truth.SilentFrame) {
		s.truth.SilentFrame[idx] = IsSilentFrame(idx)
	}
	pcm := make([]byte, s.p.FrameSamples*2)
	if IsSilentFrame(idx) {
		for i := 0; i < s.p.FrameSamples; i++ {
			dither := int16((i + idx) & 1) // values 0..1 LSB
			binary.LittleEndian.PutUint16(pcm[i*2:], uint16(dither))
		}
		return PCMFrame{Index: idx, PCM: pcm}
	}
	amp := 12000.0
	step := 2 * math.Pi * 440 / float64(s.p.SampleRate)
	for i := 0; i < s.p.FrameSamples; i++ {
		v := int16(amp * math.Sin(step*float64(i)))
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(v))
	}
	return PCMFrame{Index: idx, PCM: pcm}
}

// TicksPerFrame converts the audio frame duration to 90 kHz ticks.
func (p AudioParams) TicksPerFrame() int64 {
	return int64(p.FrameSamples) * 90000 / int64(p.SampleRate)
}
