package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"hash/crc32"
	"io"
	"os"
)

// chunkHasher duplicates every persisted byte into SHA-256 and CRC32 hashers.
type chunkHasher struct {
	w   io.Writer
	sha hash.Hash
	crc hash.Hash32
	n   int64
}

func newChunkHasher(f *os.File) *chunkHasher {
	sha := sha256.New()
	crc := crc32.NewIEEE()
	return &chunkHasher{w: io.MultiWriter(f, sha, crc), sha: sha, crc: crc}
}

func (h *chunkHasher) Write(p []byte) (int, error) {
	n, err := h.w.Write(p)
	h.n += int64(n)
	return n, err
}

func (h *chunkHasher) digest() (sha string, crc uint32, size int64) {
	return hex.EncodeToString(h.sha.Sum(nil)), h.crc.Sum32(), h.n
}
