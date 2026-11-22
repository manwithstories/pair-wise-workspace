package testfixtures

import "encoding/binary"

// Corruption 标识四类自生成边界输入。
type Corruption string

const (
	CorruptWrap      Corruption = "timestamp_wrap"
	CorruptTruncate  Corruption = "truncated_tail"
	CorruptBadBox    Corruption = "bad_box"
	CorruptIndexGap  Corruption = "index_missing"
	// CorruptContainer 破坏 ftyp 起始，使输入无法作为 ISO BMFF 容器识别。
	CorruptContainer Corruption = "container_fatal"
)

// BreakContainer 用垃圾字节覆盖文件头，制造无法识别容器的致命输入。
func BreakContainer(data []byte) []byte {
	out := append([]byte(nil), data...)
	for i := 0; i < 8 && i < len(out); i++ {
		out[i] = 0x7f
	}
	return out
}

// TruncateTail 在最后一个完整盒之后截断，模拟素材尾部损坏。
// 保留 mdat 全部样本，仅砍掉 moov 尾部之外的内容由调用方决定；
// 这里直接在给定字节数处切断。
func TruncateTail(data []byte, keep int) []byte {
	if keep >= len(data) {
		return append([]byte(nil), data...)
	}
	return append([]byte(nil), data[:keep]...)
}

// CorruptBoxHeader 把 offset 处盒的声明长度改成越界值，制造坏盒。
func CorruptBoxHeader(data []byte, boxOffset uint32, bogusSize uint32) []byte {
	out := append([]byte(nil), data...)
	out[boxOffset] = byte(bogusSize >> 24)
	out[boxOffset+1] = byte(bogusSize >> 16)
	out[boxOffset+2] = byte(bogusSize >> 8)
	out[boxOffset+3] = byte(bogusSize)
	return out
}

// StripSTSS 从容器字节中移除 stss 盒，模拟关键帧索引缺页。
// 移除会让其后的 mdat 整体前移，因此同步把所有 stco chunk 偏移减去盒长，
// 保持样本绝对偏移有效。
func StripSTSS(data []byte) []byte {
	needle := []byte("stss")
	for i := 0; i+8 <= len(data); i++ {
		if string(data[i+4:i+8]) == string(needle) {
			size := int(uint32(data[i])<<24 | uint32(data[i+1])<<16 |
				uint32(data[i+2])<<8 | uint32(data[i+3]))
			if i+size > len(data) {
				return append([]byte(nil), data...)
			}
			out := make([]byte, 0, len(data)-size)
			out = append(out, data[:i]...)
			out = append(out, data[i+size:]...)
			return shiftSTCO(out, int32(-size))
		}
	}
	return append([]byte(nil), data...)
}

// shiftSTCO 把全部 stco 条目的绝对偏移加上 delta（可负）。
func shiftSTCO(data []byte, delta int32) []byte {
	needle := []byte("stco")
	for i := 0; i+8 <= len(data); i++ {
		if string(data[i+4:i+8]) != string(needle) {
			continue
		}
		count := int(binary.BigEndian.Uint32(data[i+12 : i+16]))
		entryBase := i + 16
		for k := 0; k < count && entryBase+4*k+4 <= len(data); k++ {
			off := entryBase + 4*k
			v := int32(binary.BigEndian.Uint32(data[off : off+4])) + delta
			binary.BigEndian.PutUint32(data[off:off+4], uint32(v))
		}
	}
	return data
}

// WrapRTPSeries 生成一组会跨越 32 位回绕的 RTP 时间戳序列。
// 返回 (时间戳, 序列号) 对，调用方据此构造数据报。
func WrapRTPSeries(clockRate uint32) []struct {
	TS  uint32
	Seq uint16
} {
	base := uint32(0xffffffff - uint32(clockRate)/50)
	out := make([]struct {
		TS  uint32
		Seq uint16
	}, 6)
	for i := range out {
		out[i].TS = base + uint32(i)*(clockRate/10) // 自然溢出回绕
		out[i].Seq = uint16(i)
	}
	return out
}
