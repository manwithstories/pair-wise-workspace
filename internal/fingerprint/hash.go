// Package fingerprint builds canonical content fingerprints and deduplicates
// payload blobs. Two frames with identical normalized payload share one
// stored reference even when their metadata differs; timeline positions are
// always preserved.
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"

	"medpipe/internal/model"
)

// Canonical normalizes a raw payload before hashing: trailing zero padding
// introduced by transport buffers is stripped, and all bytes pass through a
// single deterministic encoding. The transformation never depends on
// metadata, so equal payloads always map to equal fingerprints.
func Canonical(payload []byte) []byte {
	end := len(payload)
	for end > 0 && payload[end-1] == 0 {
		end--
	}
	out := make([]byte, end)
	copy(out, payload[:end])
	return out
}

// Of returns the hex SHA-256 of the canonical payload.
func Of(payload []byte) string {
	sum := sha256.Sum256(Canonical(payload))
	return hex.EncodeToString(sum[:])
}

// Entry records one unique blob plus every frame sequence that references it.
type Entry struct {
	Digest   string
	Payload  []byte
	RefCount int
	Seqs     []uint32
}

// Catalog is the streaming dedup table. It is safe for sequential use within
// one pipeline run; bounded memory is enforced by the pipeline layer.
type Catalog struct {
	order   []string
	entries map[string]*Entry
}

// NewCatalog returns an empty dedup table.
func NewCatalog() *Catalog {
	return &Catalog{entries: map[string]*Entry{}}
}

// Add fingerprints a frame, stores the blob once on first sight and returns
// the shared reference id. Existing frames are never overwritten, so frames
// with different metadata but equal payload keep their own timeline slots.
func (c *Catalog) Add(f *model.Frame) string {
	digest := Of(f.Payload)
	e, ok := c.entries[digest]
	if !ok {
		e = &Entry{Digest: digest, Payload: Canonical(f.Payload)}
		c.entries[digest] = e
		c.order = append(c.order, digest)
	}
	e.RefCount++
	e.Seqs = append(e.Seqs, f.Seq)
	return "ref:" + digest
}

// Entry looks up a stored blob by digest.
func (c *Catalog) Entry(digest string) (*Entry, bool) {
	e, ok := c.entries[digest]
	return e, ok
}

// Entries returns unique blobs in first-seen order, making summaries stable.
func (c *Catalog) Entries() []*Entry {
	out := make([]*Entry, 0, len(c.order))
	for _, d := range c.order {
		out = append(out, c.entries[d])
	}
	return out
}

// UniqueCount reports how many distinct payloads were stored.
func (c *Catalog) UniqueCount() int { return len(c.order) }

// DedupStats summarizes space reuse for pipeline metrics.
type DedupStats struct {
	TotalFrames   int
	UniquePayload int
	ReusedRefs    int
}

// Stats computes the dedup outcome.
func (c *Catalog) Stats() DedupStats {
	total := 0
	for _, e := range c.entries {
		total += e.RefCount
	}
	return DedupStats{TotalFrames: total, UniquePayload: len(c.order), ReusedRefs: total - len(c.order)}
}
