package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

// Digest hashes an entire file and returns its lowercase hex SHA-256 plus size.
func Digest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return DigestReader(f)
}

// DigestReader hashes from a reader to EOF, returning hex digest and byte count.
func DigestReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// DigestBytes hashes an in-memory payload.
func DigestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ErrMissing reports an absent prerequisite file.
var ErrMissing = errors.New("missing file")
