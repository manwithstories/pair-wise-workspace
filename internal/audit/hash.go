// Package audit computes per-fragment SHA-256 digests and a whole-run summary.
// Given identical inputs the digest set must be bit-identical across runs; this
// is what the "repeat_hash=stable" check verifies.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"

	"pairwise/ingestcheck/internal/clock"
)

// FragmentDigest records one fragment's hash and size.
type FragmentDigest struct {
	Track    uint32
	Sequence uint32
	Bytes    int
	Sha256   string
}

// Summary aggregates the fragments of a run.
type Summary struct {
	Fragments []FragmentDigest
	Rows      int
	Root      string // SHA-256 over the canonical fragment list
}

// Hasher accumulates fragment digests into a deterministic root.
type Hasher struct {
	max    int
	digest []FragmentDigest
}

// New returns a hasher bounded by max fragment digests.
func New(max int) (*Hasher, error) {
	if max <= 0 {
		return nil, clock.New(clock.CodeArgument, "audit", "non-positive digest bound")
	}
	return &Hasher{max: max}, nil
}

// Add hashes a fragment payload and appends it to the digest list.
func (h *Hasher) Add(trackID, sequence uint32, payload []byte) error {
	if len(h.digest) >= h.max {
		return clock.New(clock.CodeAuditMismatch, "audit", "fragment digest list exceeds bound")
	}
	sum := sha256.Sum256(payload)
	h.digest = append(h.digest, FragmentDigest{
		Track:    trackID,
		Sequence: sequence,
		Bytes:    len(payload),
		Sha256:   hex.EncodeToString(sum[:]),
	})
	return nil
}

// Root computes the canonical digest over the sorted fragment list. Sorting
// by (track, sequence) makes the root independent of emission interleaving.
func (h *Hasher) Root() string {
	cp := make([]FragmentDigest, len(h.digest))
	copy(cp, h.digest)
	sort.Slice(cp, func(i, j int) bool {
		if cp[i].Track != cp[j].Track {
			return cp[i].Track < cp[j].Track
		}
		if cp[i].Sequence != cp[j].Sequence {
			return cp[i].Sequence < cp[j].Sequence
		}
		return cp[i].Sha256 < cp[j].Sha256
	})
	hsh := sha256.New()
	for _, d := range cp {
		fmt.Fprintf(hsh, "%d:%d:%d:%s\n", d.Track, d.Sequence, d.Bytes, d.Sha256)
	}
	sum := hsh.Sum(nil)
	return hex.EncodeToString(sum)
}

// Summary returns the aggregated hashes.
func (h *Hasher) Summary() Summary {
	cp := make([]FragmentDigest, len(h.digest))
	copy(cp, h.digest)
	return Summary{Fragments: cp, Rows: len(cp), Root: h.Root()}
}

// Compare reports whether two summaries are identical. It is used to prove
// repeated runs produce the same artifacts.
func Compare(a, b Summary) bool {
	if a.Rows != b.Rows || a.Root != b.Root {
		return false
	}
	for i := range a.Fragments {
		if a.Fragments[i] != b.Fragments[i] {
			return false
		}
	}
	return true
}

// DigestReader hashes a reader in bounded chunks so large inputs do not get
// loaded whole.
func DigestReader(r io.Reader, chunk int) (string, error) {
	if chunk <= 0 {
		return "", clock.New(clock.CodeArgument, "audit", "non-positive chunk size")
	}
	h := sha256.New()
	buf := make([]byte, chunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", clock.Wrap(clock.CodeIO, "audit.read", -1, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
