package pipeline

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"

	"mediapipe/internal/fingerprint"
)

const demoSampleRate = 48_000
const demoSegmentSeconds = 10

// DemoAudio builds 10-second blocks. Blocks 0 and 1 are intentionally identical
// and each contains one long silence; block 2 is different and non-silent.
// Therefore a 30-second demo has exactly two silence regions and one repeated
// segment pair.
func DemoAudio(seconds int) []float64 {
	if seconds <= 0 {
		seconds = 30
	}
	n := seconds * demoSampleRate
	out := make([]float64, n)
	blockSamples := demoSegmentSeconds * demoSampleRate
	for i := 0; i < n; i++ {
		block := i / blockSamples
		local := float64(i%blockSamples) / float64(demoSampleRate)
		silent := block <= 1 && local >= 2.0 && local < 2.8
		if !silent {
			freq := 440.0
			if block >= 2 {
				freq = 520
			}
			out[i] = 0.35 * math.Sin(2*math.Pi*freq*local)
		}
	}
	return out
}

// DemoFrameSamples returns deterministic sampled frame features. Segment 1 is
// deliberately the same title/feature sequence as segment 0.
func DemoFrameSamples(segmentIndex int, seed int64) []fingerprint.FrameSample {
	content := segmentIndex
	if segmentIndex == 1 {
		content = 0
	}
	frames := make([]fingerprint.FrameSample, 0, 7)
	for k := 0; k < 7; k++ {
		frames = append(frames, fingerprint.FrameSample{
			PTSSec: float64(k) * 1.37,
			Data:   []byte(fmt.Sprintf("seed=%d content=%d frame=%d", seed, content, k)),
		})
	}
	return frames
}

func DigestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func DigestFloats(xs []float64) string {
	h := sha256.New()
	var raw [8]byte
	for _, x := range xs {
		binary.LittleEndian.PutUint64(raw[:], math.Float64bits(x))
		_, _ = h.Write(raw[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func audioBlock(audio []float64, index, blockSeconds int) []float64 {
	if blockSeconds <= 0 {
		blockSeconds = demoSegmentSeconds
	}
	block := blockSeconds * demoSampleRate
	start := index * block
	if start >= len(audio) {
		return nil
	}
	end := start + block
	if end > len(audio) {
		end = len(audio)
	}
	return audio[start:end]
}
