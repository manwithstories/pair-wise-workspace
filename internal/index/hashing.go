package index

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
)

// chainHash 把输出建模为有序的块哈希链：最终校验和为
// SHA256(h(prev) || h(block1) || h(block2) || ...)，
// 因而可从检查点保存的中间 SHA 无损恢复，保证续跑幂等。
type chainHash struct {
	state []byte // 上一轮的 SHA256 摘要（32 字节）
}

func newChainHash() hash.Hash {
	return &chainHash{state: make([]byte, 32)}
}

// resumeHash 从持久化的十六进制中间摘要重建哈希链。
func resumeHash(hexSHA string) hash.Hash {
	state := make([]byte, 32)
	if b, err := hex.DecodeString(hexSHA); err == nil && len(b) == 32 {
		copy(state, b)
	}
	return &chainHash{state: state}
}

func (c *chainHash) Write(p []byte) (int, error) {
	blockSum := sha256.Sum256(p)
	combined := make([]byte, 0, 64)
	combined = append(combined, c.state...)
	combined = append(combined, blockSum[:]...)
	next := sha256.Sum256(combined)
	c.state = next[:]
	return len(p), nil
}

func (c *chainHash) Sum(b []byte) []byte {
	return append(b, c.state...)
}

func (c *chainHash) Reset()         { c.state = make([]byte, 32) }
func (c *chainHash) Size() int      { return sha256.Size }
func (c *chainHash) BlockSize() int { return sha256.BlockSize }
