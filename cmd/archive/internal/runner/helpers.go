package runner

import "crypto/sha256"

// sha256Bytes hashes one packet payload.
func sha256Bytes(b []byte) [32]byte { return sha256.Sum256(b) }
