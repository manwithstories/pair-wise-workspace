package render

import (
	"crypto/sha256"
	"encoding/hex"
)

// sha256Hex 返回字节内容的 SHA-256 十六进制摘要，用于帧与片段核验。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
