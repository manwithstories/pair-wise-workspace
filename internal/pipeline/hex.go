package pipeline

import "encoding/hex"

// hexSum renders a raw SHA-256 digest as the lowercase hex string used by the
// remux writer's Result.SHA256.
func hexSum(b []byte) string {
	return hex.EncodeToString(b)
}
