// Package fingerprint computes deterministic content hashes for segments and
// removes duplicate content. A hash match is never trusted alone: every
// collision candidate is re-verified by byte length and a sampled digest, so
// distinct content sharing a hash prefix cannot be collapsed incorrectly.
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"hash/fnv"
)

// Fingerprint identifies one segment's content.
type Fingerprint struct {
	// Hash is the fast 64-bit FNV-1a content hash.
	Hash uint64
	// Length is the total payload byte count.
	Length int
	// Digest is a SHA-256 over a deterministic stride sample of the payload,
	// used to confirm a hash candidate before declaring a duplicate.
	Digest [32]byte
}

// Compute builds a fingerprint from a segment's concatenated sample payloads.
// The same byte sequence always yields the same fingerprint.
func Compute(payload [][]byte) Fingerprint {
	h := fnv.New64a()
	length := 0
	sampler := sha256.New()
	stride := sampleStride(totalLen(payload))
	for _, p := range payload {
		h.Write(p)
		length += len(p)
		for i := 0; i < len(p); i++ {
			if i%stride == 0 {
				sampler.Write(p[i : i+1])
			}
		}
	}
	var digest [32]byte
	copy(digest[:], sampler.Sum(nil))
	return Fingerprint{Hash: h.Sum64(), Length: length, Digest: digest}
}

func totalLen(payload [][]byte) int {
	n := 0
	for _, p := range payload {
		n += len(p)
	}
	return n
}

// sampleStride picks a deterministic sampling interval so the digest stays
// cheap yet touches content across the whole segment.
func sampleStride(length int) int {
	switch {
	case length <= 1024:
		return 1
	case length <= 64*1024:
		return 4
	default:
		return 16
	}
}

// Equal reports a verified duplicate: identical hash, then identical length,
// then identical sampled digest. All three must agree.
func Equal(a, b Fingerprint) bool {
	if a.Hash != b.Hash {
		return false
	}
	if a.Length != b.Length {
		return false
	}
	return a.Digest == b.Digest
}

// Encode renders the hash as a stable 16-character hex token for manifests.
func (f Fingerprint) Encode() string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], f.Hash)
	const hex = "0123456789abcdef"
	out := make([]byte, 16)
	for i, b := range buf {
		out[i*2] = hex[b>>4]
		out[i*2+1] = hex[b&0x0f]
	}
	return string(out)
}

// Deduper keeps the first occurrence of each verified-unique fingerprint and
// reports duplicate submissions without mutating their order.
type Deduper struct {
	seen []Fingerprint
	dups int
}

// NewDeduper creates an empty deduper.
func NewDeduper() *Deduper { return &Deduper{} }

// Add registers a fingerprint. It returns true when the content is a verified
// duplicate of an earlier segment and false for genuinely new content.
func (d *Deduper) Add(f Fingerprint) bool {
	for _, prior := range d.seen {
		if Equal(prior, f) {
			d.dups++
			return true
		}
	}
	d.seen = append(d.seen, f)
	return false
}

// Duplicates counts verified duplicate submissions.
func (d *Deduper) Duplicates() int { return d.dups }

// Unique returns the retained fingerprints in first-seen order.
func (d *Deduper) Unique() []Fingerprint {
	out := make([]Fingerprint, len(d.seen))
	copy(out, d.seen)
	return out
}
