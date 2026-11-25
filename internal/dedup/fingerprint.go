// Package dedup computes timestamp-independent content fingerprints and
// verifies hash hits byte-for-byte before treating content as duplicate.
package dedup

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"mediad/internal/codec"
)

// Fingerprint is a content identity over codec configuration and AU bytes.
type Fingerprint struct {
	SHA256 [32]byte    `json:"sha256"`
	Codec  codec.Codec `json:"codec"`
	Size   int         `json:"size"`
}

func (f Fingerprint) String() string { return hex.EncodeToString(f.SHA256[:]) }

// Content bundles the decoder configuration and one access unit.
type Content struct {
	Codec  codec.Codec
	Config []byte
	AU     []byte
}

// Compute hashes codec identity, configuration, and AU payload. Timestamps and
// sequence locations are deliberately absent.
func Compute(c Content) Fingerprint {
	h := sha256.New()
	writeField(h, []byte(c.Codec))
	writeField(h, c.Config)
	writeField(h, c.AU)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return Fingerprint{SHA256: sum, Codec: c.Codec, Size: len(c.AU)}
}

func writeField(h interface{ Write([]byte) (int, error) }, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(b)
}

// Ref points to a previously accepted content item.
type Ref struct {
	Fingerprint Fingerprint
	Location    string `json:"location"`
}

// Loader retrieves exact prior content for collision verification.
type Loader func(Ref) (Content, error)

// Store keeps fingerprint buckets. It never deletes bytes itself; callers use
// CheckAndAdd's duplicate result to skip redundant storage.
type Store struct {
	refs map[[32]byte][]Ref

	DuplicateAUs   int64
	SavedBytes     int64
	HashCollisions int64
}

func NewStore() *Store {
	return &Store{refs: make(map[[32]byte][]Ref)}
}

func (s *Store) Refs(sum [32]byte) []Ref {
	return append([]Ref(nil), s.refs[sum]...)
}

// CheckAndAdd verifies every hash hit byte-for-byte. A cryptographic hash
// collision is added as a distinct item and is never reported duplicate.
func (s *Store) CheckAndAdd(fp Fingerprint, current Content, location string, load Loader) (bool, int64, error) {
	bucket := s.refs[fp.SHA256]
	for _, ref := range bucket {
		if ref.Fingerprint.Codec != fp.Codec || ref.Fingerprint.Size != fp.Size {
			continue
		}
		if load == nil {
			return false, 0, errors.New("dedup loader is required for byte verification")
		}
		prior, err := load(ref)
		if err != nil {
			return false, 0, fmt.Errorf("load prior content for %s: %w", ref.Location, err)
		}
		if contentEqual(prior, current) {
			s.DuplicateAUs++
			s.SavedBytes += int64(fp.Size)
			return true, int64(fp.Size), nil
		}
		s.HashCollisions++
	}

	s.refs[fp.SHA256] = append(s.refs[fp.SHA256], Ref{Fingerprint: fp, Location: location})
	return false, 0, nil
}

func contentEqual(a, b Content) bool {
	if a.Codec != b.Codec || len(a.AU) != len(b.AU) || len(a.Config) != len(b.Config) {
		return false
	}
	for i := range a.Config {
		if a.Config[i] != b.Config[i] {
			return false
		}
	}
	for i := range a.AU {
		if a.AU[i] != b.AU[i] {
			return false
		}
	}
	return true
}

// ManifestHash returns the stable SHA-256 hex digest of manifest bytes.
func ManifestHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
