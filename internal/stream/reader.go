// Package stream provides bounded local reads with SHA-256 verification.
package stream

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrTooLarge marks reads that exceed the configured byte bound.
var ErrTooLarge = errors.New("stream: file exceeds byte bound")

// ReadBounded reads path fully into memory, failing with ErrTooLarge when
// the file is larger than maxBytes.
func ReadBounded(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: %s", ErrTooLarge, path)
	}
	return data, nil
}

// SHA256Hex returns the lowercase hex SHA-256 digest of data.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// VerifySHA256 reports whether data matches the expected hex digest.
func VerifySHA256(data []byte, wantHex string) bool {
	return SHA256Hex(data) == wantHex
}
