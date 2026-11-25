package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// HashBytes returns the lowercase SHA-256 of b.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HashFile returns size and SHA-256 of path.
func HashFile(path string) (int, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return int(n), "", err
	}
	return int(n), hex.EncodeToString(h.Sum(nil)), nil
}
