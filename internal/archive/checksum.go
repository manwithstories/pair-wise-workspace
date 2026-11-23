// Package archive aggregates pipeline outputs into a single verifiable
// checksum. Identical inputs, parameters and node versions always yield
// the same checksum.
package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Checksum returns the sha256 (64 hex chars) of the canonical JSON
// encoding of v. Struct field order and Go's sorted map keys make the
// encoding deterministic for a given value.
func Checksum(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// MustChecksum is Checksum for values known to be marshalable.
func MustChecksum(v any) string {
	s, err := Checksum(v)
	if err != nil {
		panic(err)
	}
	return s
}
