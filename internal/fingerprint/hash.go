// Package fingerprint 为片段生成内容指纹。
//
// 指纹同时覆盖片段字节与时间范围：同样的字节出现在不同时间区间会
// 得到不同摘要，从而让缓存能识别“同内容、不同剪辑位置”的片段。
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"mediad/internal/model"
)

// SegmentHash 计算片段内容指纹。
//
// 输入依次为：片段字节、轨道 ID、起止 tick（int64 大端）、片段状态。
// 任一字节变化或时间范围漂移都会改变摘要。
func SegmentHash(data []byte, seg model.Segment) string {
	h := sha256.New()
	h.Write(data)
	_ = binary.Write(h, binary.BigEndian, seg.StartTick)
	_ = binary.Write(h, binary.BigEndian, seg.EndTick)
	h.Write([]byte(seg.TrackID))
	h.Write([]byte(seg.Status))
	return hex.EncodeToString(h.Sum(nil))
}

// SourceDigest 组合多个来源摘要，生成稳定的来源组合指纹。
// 顺序敏感：调用方须保证 sources 顺序确定（流水线按清单顺序传入）。
func SourceDigest(sources []model.Source) string {
	h := sha256.New()
	for _, s := range sources {
		h.Write([]byte(s.ID))
		h.Write([]byte(s.Kind))
		h.Write([]byte(s.Digest))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FileDigest 计算整个输入文件的 SHA-256（十六进制）。
func FileDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Verify 复核片段指纹是否与期望摘要一致，不一致返回 false。
func Verify(data []byte, seg model.Segment, want string) bool {
	return SegmentHash(data, seg) == want
}

// MustHex 是测试辅助：把整数编码为定长十六进制标签。
func MustHex(v uint64) string {
	return fmt.Sprintf("%016x", v)
}
