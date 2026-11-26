package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"relay/internal/stream"
)

// ManifestHash folds every committed fragment's key and output hash into one
// digest in deterministic key order. Two runs over identical inputs and
// parameters produce the same manifest hash only when their full repaired
// timelines and outputs are byte-identical.
func ManifestHash(cp *stream.Checkpoint) string {
	entries := cp.Entries()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e.Key))
		h.Write([]byte{0})
		h.Write([]byte(e.OutputHash))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// IndexHash returns the hash of an index file's serialized bytes. Because
// index.Write uses canonical ordering and formatting, this stays stable
// across repeated runs.
func IndexHash(serialized []byte) string {
	sum := sha256.Sum256(serialized)
	return hex.EncodeToString(sum[:])
}
