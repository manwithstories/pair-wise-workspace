package testkit

import (
	"crypto/sha256"
	"fmt"
)

func seedByte(seed int64) byte {
	sum := sha256.Sum256([]byte(fmt.Sprint(seed)))
	return sum[0]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
