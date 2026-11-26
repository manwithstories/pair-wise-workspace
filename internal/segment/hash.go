// Package segment 按固定包数切片计算独立 SHA-256，并建立关键帧索引、
// 核对随机定位命中率。哈希只依赖规范化后的包字节，保证可复算。
package segment

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// DefaultPacketsPerSegment 是每个片段固定包含的 TS 包数（188 字节/包）。
const DefaultPacketsPerSegment = 32

// MaxSegments 限制片段总数，防止异常输入产生无界索引。
const MaxSegments = 4096

// ErrSegment 表示切片参数或边界不合法。
var ErrSegment = errors.New("segment: invalid segmentation")

// Hash 是单个片段的独立摘要。
type Hash struct {
	Index      int    `json:"index"`
	PacketFrom int    `json:"packet_from"`
	PacketTo   int    `json:"packet_to"` // 不含
	Packets    int    `json:"packets"`
	SHA256     string `json:"sha256"`
}

// Hasher 按固定包数切片并逐段哈希。
type Hasher struct {
	perSegment int
}

// NewHasher 创建分片器，perSegment 非法时回退默认值并报错。
func NewHasher(perSegment int) (*Hasher, error) {
	if perSegment <= 0 {
		return nil, fmt.Errorf("%w: perSegment=%d", ErrSegment, perSegment)
	}
	return &Hasher{perSegment: perSegment}, nil
}

// HashPackets 把规范化后的 188 字节包数组切成固定大小片段并独立哈希。
// 最后一个片段允许不足 perSegment。
func (h *Hasher) HashPackets(packets [][]byte) ([]Hash, error) {
	if len(packets) == 0 {
		return nil, fmt.Errorf("%w: no packets", ErrSegment)
	}
	n := (len(packets) + h.perSegment - 1) / h.perSegment
	if n > MaxSegments {
		return nil, fmt.Errorf("%w: %d > %d", ErrSegment, n, MaxSegments)
	}
	out := make([]Hash, 0, n)
	for from := 0; from < len(packets); from += h.perSegment {
		to := from + h.perSegment
		if to > len(packets) {
			to = len(packets)
		}
		sum := sha256.New()
		for _, p := range packets[from:to] {
			if len(p) == 0 {
				return nil, fmt.Errorf("%w: empty packet at %d", ErrSegment, from)
			}
			sum.Write(p)
		}
		out = append(out, Hash{
			Index:      len(out),
			PacketFrom: from,
			PacketTo:   to,
			Packets:    to - from,
			SHA256:     hex.EncodeToString(sum.Sum(nil)),
		})
	}
	return out, nil
}

// Verify 用期望摘要逐段比对，返回不匹配的片段下标（空表示全部一致）。
func Verify(got, want []Hash) []int {
	var mismatch []int
	if len(got) != len(want) {
		// 长度不同：所有期望下标都算不匹配。
		for i := range want {
			mismatch = append(mismatch, i)
		}
		return mismatch
	}
	for i := range got {
		if got[i].SHA256 != want[i].SHA256 ||
			got[i].PacketFrom != want[i].PacketFrom || got[i].PacketTo != want[i].PacketTo {
			mismatch = append(mismatch, i)
		}
	}
	return mismatch
}
