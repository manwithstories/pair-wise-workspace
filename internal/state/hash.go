// Package state persists resumable job progress. Every segment is written to
// a temporary file, hash-verified and only then atomically committed; the
// journal records the committed hash. On recovery a checkpoint is reused only
// when the referenced file exists and its hash still matches, so a missing or
// corrupted segment is transparently re-encoded instead of trusted.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// FileSHA256 streams a file's content through SHA-256 without loading it into
// memory, keeping the resident footprint independent of segment size.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashBytes returns the hex SHA-256 of an in-memory payload.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
