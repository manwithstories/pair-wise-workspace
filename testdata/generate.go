// Package testdata 用确定性伪随机生成现场多机位样例：MP4/MOV 容器、
// 自生成 RTP 包与 PCM 音频。所有字节仅依赖 seed，保证重跑可核验。
package testdata

import (
	"encoding/binary"
	"math/rand"
)

// RNG 包装确定性随机源，供各生成函数共享同一序列。
type RNG struct{ r *rand.Rand }

// NewRNG 以 seed 构造确定性随机源。
func NewRNG(seed int64) *RNG { return &RNG{r: rand.New(rand.NewSource(seed))} }

// Intn 返回 [0,n) 确定随机整数。
func (g *RNG) Intn(n int) int { return g.r.Intn(n) }

// Byte 填充并返回确定性随机字节切片。
func (g *RNG) Byte(n int) []byte {
	b := make([]byte, n)
	_, _ = g.r.Read(b)
	return b
}

// U16/U32 生成确定随机标量。
func (g *RNG) U16() uint16 { return uint16(g.r.Uint32() & 0xffff) }
func (g *RNG) U32() uint32 { return g.r.Uint32() }

// Box 手工构造一个 ISO BMFF 盒。
type Box struct {
	Type    string
	Payload []byte
	Child   []*Box
}

// Encode 把盒序列化为字节（容器盒的 Payload 与 Child 拼接）。
func (b *Box) Encode() []byte {
	var payload []byte
	payload = append(payload, b.Payload...)
	for _, c := range b.Child {
		payload = append(payload, c.Encode()...)
	}
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:8], b.Type)
	copy(out[8:], payload)
	return out
}

// FullBoxPrefix 生成 version+flags 四字节。
func FullBoxPrefix(version byte, flags uint32) []byte {
	return []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
}

// U32b/U64b 大端编码辅助。
func U32b(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

// U64b 大端编码 64 位值。
func U64b(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// CString 生成以 NUL 结尾的字符串字节。
func CString(s string) []byte { return append([]byte(s), 0) }
