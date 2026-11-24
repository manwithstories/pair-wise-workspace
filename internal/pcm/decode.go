// Package pcm decodes big-endian linear PCM (L16) into the shared float
// representation used by all DSP stages.
package pcm

import (
	"encoding/binary"
	"fmt"
	"math"

	"mediad/internal/fault"
	"mediad/internal/reorder"
)

// Audio is interleaved float64 PCM in [-1,1).
type Audio struct {
	SampleRate int
	Channels   int
	Frames     int
	Samples    []float64 // length == Frames*Channels
}

// NewAudio allocates an exactly sized buffer and checks memory arithmetic.
func NewAudio(rate, channels, frames int) (*Audio, error) {
	if rate <= 0 || channels <= 0 || frames < 0 {
		return nil, fmt.Errorf("invalid audio format rate=%d channels=%d frames=%d", rate, channels, frames)
	}
	if frames > (math.MaxInt / channels) {
		return nil, fmt.Errorf("audio allocation overflow: frames=%d channels=%d", frames, channels)
	}
	n := frames * channels
	if n > math.MaxInt/8 {
		return nil, fmt.Errorf("audio buffer too large: %d samples", n)
	}
	// float64 storage plus conversion headroom must stay within the documented
	// resident-memory envelope.
	if int64(n)*8 > fault.MaxMemoryBytes/2 {
		return nil, fmt.Errorf("audio buffer estimate exceeds %d MiB memory limit", fault.MaxMemoryBytes>>20)
	}
	return &Audio{SampleRate: rate, Channels: channels, Frames: frames, Samples: make([]float64, n)}, nil
}

// DecodeL16 decodes one big-endian signed L16 packet.
func DecodeL16(payload []byte, channels int) (samples []float64, frames int, err error) {
	if channels <= 0 {
		return nil, 0, fmt.Errorf("invalid channel count %d", channels)
	}
	if len(payload)%2 != 0 {
		return nil, 0, fmt.Errorf("L16 payload has odd length %d", len(payload))
	}
	frameBytes := channels * 2
	if frameBytes <= 0 || len(payload)%frameBytes != 0 {
		return nil, 0, fmt.Errorf("L16 payload length %d is not aligned to %d channels", len(payload), channels)
	}
	frames = len(payload) / frameBytes
	samples = make([]float64, len(payload)/2)
	for i := 0; i < len(payload); i += 2 {
		v := int16(binary.BigEndian.Uint16(payload[i : i+2]))
		samples[i/2] = float64(v) / 32768.0
	}
	return samples, frames, nil
}

// Silence returns an interleaved zero buffer. Exact zeros are important:
// later gain stages must never turn a missing-packet concealment region into
// synthesized noise.
func Silence(rate, channels, frames int) (*Audio, error) {
	return NewAudio(rate, channels, frames)
}

// DecodeBlocks concatenates ordered blocks. Missing blocks become exact
// silence using the shared timeline's packet duration.
func DecodeBlocks(blocks []reorder.Block, rate, channels int) (*Audio, int, error) {
	totalFrames := 0
	missingFrames := 0
	for _, b := range blocks {
		if b.Samples <= 0 {
			return nil, missingFrames, fmt.Errorf("block %d has invalid duration %d", b.ExtSeq, b.Samples)
		}
		totalFrames += b.Samples
		if b.Missing {
			missingFrames += b.Samples
		}
	}
	out, err := NewAudio(rate, channels, totalFrames)
	if err != nil {
		return nil, missingFrames, err
	}

	pos := 0
	for _, b := range blocks {
		n := b.Samples * channels
		if b.Missing {
			// NewAudio already zeroed this range.
			pos += n
			continue
		}
		samples, frames, err := DecodeL16(b.Payload, channels)
		if err != nil {
			return nil, missingFrames, fmt.Errorf("decode seq=%d: %w", b.Sequence, err)
		}
		if frames != b.Samples {
			return nil, missingFrames, fmt.Errorf("seq=%d duration mismatch: %d != %d", b.Sequence, frames, b.Samples)
		}
		copy(out.Samples[pos:pos+len(samples)], samples)
		pos += len(samples)
	}
	return out, missingFrames, nil
}

// ToS16BE converts normalized float PCM to interleaved signed big-endian L16.
func (a *Audio) ToS16BE() []byte {
	out := make([]byte, len(a.Samples)*2)
	for i, x := range a.Samples {
		if x > 1 {
			x = 1
		} else if x < -1 {
			x = -1
		}
		v := int16(math.Round(x * 32767))
		binary.BigEndian.PutUint16(out[i*2:i*2+2], uint16(v))
	}
	return out
}

// Clone returns an independent buffer.
func (a *Audio) Clone() *Audio {
	cp := make([]float64, len(a.Samples))
	copy(cp, a.Samples)
	return &Audio{SampleRate: a.SampleRate, Channels: a.Channels, Frames: a.Frames, Samples: cp}
}
