package core

import "crypto/sha256"

func hashOf(b []byte) [32]byte { return sha256.Sum256(b) }
