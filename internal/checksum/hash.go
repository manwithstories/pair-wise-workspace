// Package checksum computes SHA-256 digests for inputs, outputs and
// per-track compressed sample sequences.
package checksum

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"io"
	"os"
)

// File hashes the complete contents of path and returns its size.
func File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Reader hashes everything read from r.
func Reader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Prefix hashes exactly n bytes of the file at path.
func Prefix(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, f, n); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SampleHasher hashes a sequence of samples with length framing so that
// concatenation ambiguity cannot hide a mismatch.
type SampleHasher struct {
	h     hash.Hash
	count int
	bytes int64
}

func NewSampleHasher() *SampleHasher {
	return &SampleHasher{h: sha256.New()}
}

func (s *SampleHasher) Add(data []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(data)))
	_, _ = s.h.Write(size[:])
	_, _ = s.h.Write(data)
	s.count++
	s.bytes += int64(len(data))
}

func (s *SampleHasher) Count() int { return s.count }

func (s *SampleHasher) Bytes() int64 { return s.bytes }

func (s *SampleHasher) Sum() string { return hex.EncodeToString(s.h.Sum(nil)) }
