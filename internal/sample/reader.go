// Package sample reads sample bytes via bounded ReaderAt sections and
// computes per-frame SHA-256 digests.
package sample

import (
	"crypto/sha256"
	"encoding/hex"
	"io"

	"mp4tool/internal/index"
)

// HashSample streams one sample through SHA-256 without loading the
// whole file; reads are bounded to the sample extent.
func HashSample(r io.ReaderAt, f index.Frame) (string, error) {
	sr := io.NewSectionReader(r, int64(f.Offset), int64(f.Size))
	h := sha256.New()
	if _, err := io.Copy(h, sr); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ReadSample copies one sample's raw bytes to w, returning its SHA-256.
func ReadSample(r io.ReaderAt, f index.Frame, w io.Writer) (string, int64, error) {
	sr := io.NewSectionReader(r, int64(f.Offset), int64(f.Size))
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), sr)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
