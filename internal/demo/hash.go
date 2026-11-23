package demo

import (
	"crypto/sha256"
	"encoding/hex"
)

// sum256 returns the SHA-256 digest of data.
func sum256(data []byte) [32]byte {
	return sha256.Sum256(data)
}

// hexEncode renders a 32-byte digest as 64 lowercase hex characters.
func hexEncode(sum [32]byte) string {
	return hex.EncodeToString(sum[:])
}
