package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hashCanonical(v any) (string, []byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	return hashBytes(b), b, nil
}
