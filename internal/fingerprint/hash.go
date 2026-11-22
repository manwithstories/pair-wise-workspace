// Package fingerprint compares windowed content hashes across tracks
// to find reused media. The earliest track keeps the content; hash
// matches are always verified against the raw samples so a collision
// cannot produce a false duplicate, and silent windows are excluded
// so muted ranges cannot make whole tracks look identical.
package fingerprint

import (
	"encoding/binary"
	"hash/fnv"

	"mediacore/internal/clock"
)

// hashWindow maps one window of samples to a 64-bit fingerprint.
// It is a variable so tests can force collisions.
var hashWindow = func(samples []int16) uint64 {
	h := fnv.New64a()
	var b [2]byte
	for _, s := range samples {
		binary.LittleEndian.PutUint16(b[:], uint16(s))
		h.Write(b[:])
	}
	return h.Sum64()
}

// Hasher computes window hashes incrementally; memory stays bounded
// by one window plus the hash list.
type Hasher struct {
	window int
	buf    []int16
	hashes []uint64
}

// NewHasher returns a streaming window hasher.
func NewHasher(windowSamples int) (*Hasher, error) {
	if windowSamples <= 0 {
		return nil, clock.At(clock.CodeManifestInvalid, "fingerprint", "", -1,
			"window %d samples", windowSamples)
	}
	return &Hasher{window: windowSamples}, nil
}

// Feed consumes samples; only complete windows are hashed.
func (h *Hasher) Feed(samples []int16) {
	h.buf = append(h.buf, samples...)
	for len(h.buf) >= h.window {
		h.hashes = append(h.hashes, hashWindow(h.buf[:h.window]))
		h.buf = append([]int16(nil), h.buf[h.window:]...)
	}
}

// Hashes returns the hashes of all complete windows so far.
func (h *Hasher) Hashes() []uint64 { return h.hashes }

// Duplicate marks one window whose content repeats an earlier track.
type Duplicate struct {
	WindowIndex int
	Earliest    string // track that keeps the content
	RepeatedBy  string // track that repeats it
}

// FindDuplicates compares two time-aligned hash series. Every hash
// match is verified against the raw window bytes; silent windows are
// skipped. ref is the earlier (kept) source.
func FindDuplicates(earliest, repeated string, refHashes, dupHashes []uint64,
	refSamples, dupSamples []int16, window int, silent func(windowIndex int) bool) []Duplicate {
	n := len(refHashes)
	if len(dupHashes) < n {
		n = len(dupHashes)
	}
	var out []Duplicate
	for i := 0; i < n; i++ {
		if silent != nil && silent(i) {
			continue
		}
		if refHashes[i] != dupHashes[i] {
			continue
		}
		// Collision candidate: verify against raw samples.
		a := windowSlice(refSamples, i, window)
		b := windowSlice(dupSamples, i, window)
		if a == nil || b == nil || !equalSamples(a, b) {
			continue
		}
		out = append(out, Duplicate{
			WindowIndex: i,
			Earliest:    earliest,
			RepeatedBy:  repeated,
		})
	}
	return out
}

func windowSlice(samples []int16, idx, window int) []int16 {
	lo := idx * window
	hi := lo + window
	if lo < 0 || hi > len(samples) {
		return nil
	}
	return samples[lo:hi]
}

func equalSamples(a, b []int16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
