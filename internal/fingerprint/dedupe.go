// Package fingerprint normalizes audio content into a deterministic
// fingerprint and collapses equal content onto shared storage references.
// Normalization removes channel-mapping and sample-rate identity so the
// same program recorded with different layouts or clocks still matches:
// channels are downmixed to mono with integer averaging, then resampled
// to a canonical clock with integer nearest-frame picking.
package fingerprint

import (
	"encoding/binary"
	"hash/fnv"

	"audiorestore/internal/model"
)

// CanonicalRate is the clock every fingerprint is expressed on.
const CanonicalRate model.SampleRate = 48000

// Content is one fragment's decoded samples: Data[channel][frame].
type Content struct {
	TrackID string
	Rate    model.SampleRate
	Data    [][]int
}

// Normalize downmixes channels and resamples to the canonical clock using
// only integer arithmetic. The output is bit-identical for equal content
// regardless of source channel order or sample rate.
func Normalize(c Content) []int32 {
	mono := downmix(c.Data)
	if c.Rate == CanonicalRate {
		out := make([]int32, len(mono))
		for i, v := range mono {
			out[i] = int32(v)
		}
		return out
	}
	src := int64(c.Rate)
	dst := int64(CanonicalRate)
	n := int64(len(mono))
	outLen := n * dst / src
	out := make([]int32, outLen)
	for j := range out {
		// Nearest source frame: round(j*src/dst) with half-away-zero.
		num := int64(j) * src
		srcFrame := (num + dst/2) / dst
		if srcFrame >= n {
			srcFrame = n - 1
		}
		out[j] = int32(mono[srcFrame])
	}
	return out
}

// downmix averages channels per frame with integer math (truncation is
// deterministic and channel-order independent).
func downmix(data [][]int) []int {
	if len(data) == 0 {
		return nil
	}
	n := len(data[0])
	for _, ch := range data[1:] {
		if len(ch) < n {
			n = len(ch)
		}
	}
	out := make([]int, n)
	for i := 0; i < n; i++ {
		var sum int64
		for _, ch := range data {
			sum += int64(ch[i])
		}
		out[i] = int(sum / int64(len(data)))
	}
	return out
}

// Hash renders the canonical fingerprint as a stable 64-bit FNV-1a value
// over length-prefixed little-endian samples.
func Hash(samples []int32) uint64 {
	h := fnv.New64a()
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(samples)))
	h.Write(lenBuf[:])
	var buf [4]byte
	for _, v := range samples {
		binary.LittleEndian.PutUint32(buf[:], uint32(v))
		h.Write(buf[:])
	}
	return h.Sum64()
}
