// Package metric provides deterministic content hashing used as idempotency
// keys and post-repair integrity evidence.
package metric

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sort"
	"strings"
)

// BytesSHA256 returns the lowercase hex SHA-256 of data.
func BytesSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ReaderSHA256 streams a hash without loading a whole segment twice.
func ReaderSHA256(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FileSHA256 hashes a local file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return ReaderSHA256(f)
}

// InputsHash builds a stable idempotency key from named inputs. Names are
// sorted so callers cannot accidentally make the key order-dependent.
func InputsHash(parts map[string]string) string {
	names := make([]string, 0, len(parts))
	for name := range parts {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		_, _ = io.WriteString(h, name)
		_, _ = h.Write([]byte{0})
		_, _ = io.WriteString(h, parts[name])
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyBytes reports whether data hashes to want.
func VerifyBytes(data []byte, want string) bool {
	return strings.EqualFold(BytesSHA256(data), want)
}
