package remux

import "crypto/sha256"

// sha256Hasher is a thin named wrapper so the content-address layer and the
// remuxer compute fragment digests with the exact same code path.
type sha256Hasher struct{ h interface {
	Write(p []byte) (int, error)
	Sum(b []byte) []byte
} }

func newSha256() *sha256Hasher { return &sha256Hasher{h: sha256.New()} }

func (s *sha256Hasher) Write(p []byte) { _, _ = s.h.Write(p) }

func (s *sha256Hasher) Sum256() [32]byte {
	var out [32]byte
	copy(out[:], s.h.Sum(nil))
	return out
}
