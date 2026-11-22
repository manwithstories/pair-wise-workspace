// Package gen produces deterministic frame manifests, RTP traces and
// bandwidth trajectories, plus self-contained corrupted samples. Every
// output is a pure function of the seed and parameters, so identical
// inputs always yield identical statistics and index summaries.
package gen

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"

	"mediascan/internal/frame"
)

// FrameParams configures the generated frame sequence.
type FrameParams struct {
	Seed       int64
	Frames     int
	FPS        int
	GOP        int
	TargetKbps int
	ClockRate  int64
}

// Generated is one frame's payload plus its manifest entry.
type Generated struct {
	Entry   frame.Frame
	Payload []byte
}

// Frames builds the deterministic frame set: sizes follow a seeded LCG
// around the target bitrate, keyframes land on the GOP grid, and the
// SHA-256 is computed over the exact payload bytes.
func Frames(p FrameParams) ([]Generated, error) {
	if p.Frames <= 0 || p.FPS <= 0 || p.GOP <= 0 || p.TargetKbps <= 0 {
		return nil, fmt.Errorf("gen: invalid frame params %+v", p)
	}
	if p.Frames > frame.MaxFrames {
		return nil, fmt.Errorf("gen: %d frames exceed limit", p.Frames)
	}
	rng := rand.New(rand.NewSource(p.Seed))
	perFrame := p.TargetKbps * 1000 / 8 / p.FPS
	out := make([]Generated, 0, p.Frames)
	var total int64
	for i := 0; i < p.Frames; i++ {
		jitter := rng.Intn(201) - 100 // +/- 100 bytes, deterministic
		size := perFrame + jitter
		if size < 64 {
			size = 64
		}
		if size > frame.MaxFrameSize {
			size = frame.MaxFrameSize
		}
		total += int64(size)
		if total > frame.MaxTotalBytes {
			return nil, fmt.Errorf("gen: total bytes exceed cap at frame %d", i)
		}
		payload := make([]byte, size)
		FillDeterministic(payload, p.Seed, int64(i))
		sum := sha256.Sum256(payload)
		out = append(out, Generated{
			Entry: frame.Frame{
				Index:     i,
				ID:        fmt.Sprintf("frame-%06d", i),
				Keyframe:  i%p.GOP == 0,
				Timestamp: int64(i) * 1000 / int64(p.FPS),
				Size:      size,
				SHA256:    hex(sum[:]),
			},
			Payload: payload,
		})
	}
	return out, nil
}

// FillDeterministic fills buf with a seed/frame-index-derived byte pattern
// without allocating per-byte randomness.
func FillDeterministic(buf []byte, seed, frameIdx int64) {
	var word [8]byte
	binary.LittleEndian.PutUint64(word[:], uint64(seed)*1099511628211^uint64(frameIdx)*1469598103934665603)
	for i := range buf {
		buf[i] = word[i%8] ^ byte(i) ^ byte(frameIdx)
	}
}

func hex(b []byte) string {
	const h = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[2*i] = h[v>>4]
		out[2*i+1] = h[v&0xF]
	}
	return string(out)
}
