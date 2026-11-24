package segment

import (
	"encoding/binary"
	"math"
)

// fMP4 构造标志位（ISO/IEC 14496-12）。
const (
	trunDataOffsetPresent      = 0x000001
	trunSampleDurationPresent  = 0x000100
	trunSampleSizePresent      = 0x000200
	trunSampleFlagsPresent     = 0x000400
	trunSampleCTOPresent       = 0x000800
	tfhdDefaultBaseIsMoofFlag  = 0x020000
	sampleFlagNonSync          = 0x00010000 // sample_depends_on=1（非 I 帧）
)

// fmp4Spec 描述一个待构造的合成 fMP4 分片。
type fmp4Spec struct {
	trackID    uint32
	baseDTS    uint64 // tfdt baseMediaDecodeTime
	durations  []uint32
	sizes      []uint32
	nonSync    []bool // true 表示该样本不是关键帧
	truncate   int    // >0 时截断到该字节数，模拟 truncated_box
	omitMdat   bool
	omitMoof   bool
	corruptBox bool // 把 moof 大小声明为越界值
}

// buildFMP4 按规范字节布局构造单 moof/mdat 分片，样本负载为可核验的填充字节。
func buildFMP4(s fmp4Spec) []byte {
	n := len(s.durations)
	// trun 负载：flags(4)+sampleCount(4)+dataOffset(4)+ 每样本 duration/size/flags（12）。
	trunPayload := 4 + 4 + 4 + n*12
	trunBox := 8 + trunPayload
	// tfhd: version/flags(4)+trackID(4)，default-base-is-moof。
	tfhdBox := 8 + 8
	// tfdt v1: version/flags(4)+baseMediaDecodeTime(8)。
	tfdtBox := 8 + 12
	trafBox := 8 + tfhdBox + tfdtBox + trunBox
	_ = trafBox // 盒大小仅用于布局理解，实际长度由 binary 追加决定

	totalSampleBytes := uint32(0)
	for _, z := range s.sizes {
		totalSampleBytes += z
	}

	var b []byte
	appendBox := func(typ string, payload []byte) {
		hdr := make([]byte, 8)
		binary.BigEndian.PutUint32(hdr, uint32(8+len(payload)))
		copy(hdr[4:], typ)
		b = append(b, hdr...)
		b = append(b, payload...)
	}

	// moof > traf
	var traf []byte
	putFullBox := func(flags uint32) []byte {
		p := make([]byte, 4)
		binary.BigEndian.PutUint32(p, flags) // version=0
		return p
	}
	{
		p := putFullBox(tfhdDefaultBaseIsMoofFlag)
		p = binary.BigEndian.AppendUint32(p, s.trackID)
		traf = appendBoxInto(traf, "tfhd", p)
	}
	{
		p := make([]byte, 4+8)
		binary.BigEndian.PutUint32(p, 0x01000000) // version=1
		binary.BigEndian.PutUint64(p[4:], s.baseDTS)
		traf = appendBoxInto(traf, "tfdt", p)
	}
	{
		p := make([]byte, 0, trunPayload)
		flags := uint32(trunDataOffsetPresent | trunSampleDurationPresent |
			trunSampleSizePresent | trunSampleFlagsPresent)
		p = binary.BigEndian.AppendUint32(p, flags)
		p = binary.BigEndian.AppendUint32(p, uint32(n))
		// dataOffset：相对 moof 起点；moof 后紧跟 mdat 头（8 字节）即负载。
		p = binary.BigEndian.AppendUint32(p, 8)
		for i := 0; i < n; i++ {
			p = binary.BigEndian.AppendUint32(p, s.durations[i])
			p = binary.BigEndian.AppendUint32(p, s.sizes[i])
			flag := uint32(0)
			if i < len(s.nonSync) && s.nonSync[i] {
				flag = sampleFlagNonSync
			}
			p = binary.BigEndian.AppendUint32(p, flag)
		}
		traf = appendBoxInto(traf, "trun", p)
	}
	moofPayload := appendBoxInto(nil, "traf", traf)
	if !s.omitMoof {
		if s.corruptBox {
			hdr := make([]byte, 8)
			binary.BigEndian.PutUint32(hdr, math.MaxUint32) // 越界大小
			copy(hdr[4:], "moof")
			b = append(b, hdr...)
			b = append(b, moofPayload...)
		} else {
			appendBox("moof", moofPayload)
		}
	}
	if !s.omitMdat {
		payload := make([]byte, totalSampleBytes)
		for i := range payload {
			payload[i] = byte(i%251 + 1) // 非零可核验填充
		}
		appendBox("mdat", payload)
	}
	if s.truncate > 0 && s.truncate < len(b) {
		b = b[:s.truncate]
	}
	return b
}

// appendBoxInto 向 dst 追加一个完整盒并返回新切片。
func appendBoxInto(dst []byte, typ string, payload []byte) []byte {
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, uint32(8+len(payload)))
	copy(hdr[4:], typ)
	dst = append(dst, hdr...)
	dst = append(dst, payload...)
	return dst
}
