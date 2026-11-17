// Package fixture 按 docs/garc-format.md 生成全部本地 GARC 测试夹具，
// 不读取任何外部媒体文件。生成器与解析器共用同一字节布局规范。
package fixture

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"

	"hdrcheck/internal/garc"
	"hdrcheck/internal/hdr"
)

// trackSpec 描述一条待写入轨道的表项。
type trackSpec struct {
	id       uint32
	typ      byte
	hdrFlag  byte
	seiCount uint32
}

// tlvChunk 是已编码的容器 TLV 记录及其文件偏移（Build 时回填）。
type tlvChunk struct {
	bytes  []byte
	offset int64
}

// seiChunk 是已编码的 SEI 记录及其文件偏移。
type seiChunk struct {
	bytes  []byte
	offset int64
}

// Builder 按段累积字节，Build 时统一分配偏移并写文件头。
type Builder struct {
	order      binary.ByteOrder
	endianByte byte
	flags      uint16
	tracks     []trackSpec
	tlvs       []tlvChunk
	seis       []seiChunk
	// 损坏注入（作用于最终字节）。
	corruptVersion  *byte
	corruptFileSize *uint32
	dropTail        int
	flipOffsets     []int64
}

// NewBuilder 按字节序构造空 Builder（1=LE，2=BE）。
func NewBuilder(endianness byte) (*Builder, error) {
	b := &Builder{endianByte: endianness}
	switch endianness {
	case garc.EndianLittle:
		b.order = binary.LittleEndian
	case garc.EndianBig:
		b.order = binary.BigEndian
	default:
		return nil, fmt.Errorf("fixture: unsupported endianness %d", endianness)
	}
	return b, nil
}

// EnableContainerHDR 置位文件头 HDR 容器标记。
func (b *Builder) EnableContainerHDR() { b.flags |= garc.FlagHDRContainer }

// AddVideoTrack 追加一条视频轨；seiCount 为该轨将写入的 SEI 记录数。
func (b *Builder) AddVideoTrack(id uint32, hdrFlag bool, seiCount uint32) {
	f := byte(0)
	if hdrFlag {
		f = 1
	}
	b.tracks = append(b.tracks, trackSpec{id: id, typ: garc.TrackTypeVideo, hdrFlag: f, seiCount: seiCount})
}

// AddMasteringTLV 写入母版显示 TLV（坐标 1e-4，亮度 1e-4 nit）。
func (b *Builder) AddMasteringTLV(m hdr.MasteringDisplay) {
	p := make([]byte, 36)
	for i := 0; i < 6; i++ {
		b.order.PutUint16(p[i*4:], m.DisplayPrimaries[i][0])
		b.order.PutUint16(p[i*4+2:], m.DisplayPrimaries[i][1])
	}
	b.order.PutUint16(p[24:], m.WhitePoint[0])
	b.order.PutUint16(p[26:], m.WhitePoint[1])
	b.order.PutUint32(p[28:], m.MinDisplayMasteringLuminance)
	b.order.PutUint32(p[32:], m.MaxDisplayMasteringLuminance)
	b.appendTLV(hdr.TagMasteringDisplay, p)
}

// AddCLLTLV 写入 MaxCLL/MaxFALL TLV（nit）。
func (b *Builder) AddCLLTLV(maxCLL, maxFALL uint16) {
	p := make([]byte, 4)
	b.order.PutUint16(p[0:], maxCLL)
	b.order.PutUint16(p[2:], maxFALL)
	b.appendTLV(hdr.TagContentLight, p)
}

// AddUnitDimensionTLV 写入量纲标记。
func (b *Builder) AddUnitDimensionTLV(dim byte) {
	b.appendTLV(hdr.TagUnitDimension, []byte{dim})
}

// AddRawTLV 追加任意原始 TLV（用于重复字段/未知 tag 夹具）。
func (b *Builder) AddRawTLV(tag uint16, payload []byte) { b.appendTLV(tag, payload) }

// AddSEICLL 写入一条 content-light-level SEI 记录。
func (b *Builder) AddSEICLL(trackID, seq, tStart, tEnd uint32, maxCLL, maxFALL uint16) {
	p := make([]byte, 4)
	b.order.PutUint16(p[0:], maxCLL)
	b.order.PutUint16(p[2:], maxFALL)
	b.appendSEI(trackID, seq, tStart, tEnd, hdr.SEIPayloadCLL, p)
}

// AddSEIMastering 写入一条 mastering-display SEI 记录。
func (b *Builder) AddSEIMastering(trackID, seq, tStart, tEnd uint32, m hdr.MasteringDisplay) {
	p := make([]byte, 36)
	for i := 0; i < 6; i++ {
		b.order.PutUint16(p[i*4:], m.DisplayPrimaries[i][0])
		b.order.PutUint16(p[i*4+2:], m.DisplayPrimaries[i][1])
	}
	b.order.PutUint16(p[24:], m.WhitePoint[0])
	b.order.PutUint16(p[26:], m.WhitePoint[1])
	b.order.PutUint32(p[28:], m.MinDisplayMasteringLuminance)
	b.order.PutUint32(p[32:], m.MaxDisplayMasteringLuminance)
	b.appendSEI(trackID, seq, tStart, tEnd, hdr.SEIPayloadMastering, p)
}

func (b *Builder) appendTLV(tag uint16, payload []byte) {
	head := make([]byte, 4)
	b.order.PutUint16(head[0:], tag)
	b.order.PutUint16(head[2:], uint16(len(payload)))
	body := append(head, payload...)
	rec := make([]byte, 0, len(body)+4)
	rec = append(rec, body...)
	rec = appendU32(rec, b.order, crc32.ChecksumIEEE(body))
	b.tlvs = append(b.tlvs, tlvChunk{bytes: rec})
}

func (b *Builder) appendSEI(trackID, seq, tStart, tEnd uint32, pType byte, payload []byte) {
	head := make([]byte, 17)
	b.order.PutUint32(head[0:], trackID)
	b.order.PutUint32(head[4:], seq)
	b.order.PutUint32(head[8:], tStart)
	b.order.PutUint32(head[12:], tEnd)
	head[16] = pType
	body := append(head, leb128(uint64(len(payload)))...)
	body = append(body, payload...)
	rec := make([]byte, 0, len(body)+4)
	rec = append(rec, body...)
	rec = appendU32(rec, b.order, crc32.ChecksumIEEE(body))
	b.seis = append(b.seis, seiChunk{bytes: rec})
}

// leb128 编码无符号 LEB128。
func leb128(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7F)
		v >>= 7
		if v != 0 {
			out = append(out, b|0x80)
		} else {
			return append(out, b)
		}
	}
}

// appendU32 以显式字节序追加一个 uint32（binary.ByteOrder 接口无 Append 方法）。
func appendU32(dst []byte, order binary.ByteOrder, v uint32) []byte {
	var buf [4]byte
	order.PutUint32(buf[:], v)
	return append(dst, buf[:]...)
}

// --- 损坏注入钩子 ---

// CorruptVersion 把文件头版本字节改为 v（触发 UNKNOWN_VERSION）。
func (b *Builder) CorruptVersion(v byte) { b.corruptVersion = &v }

// CorruptDeclaredFileSize 覆写 file_size 字段（触发 LENGTH_MISMATCH）。
func (b *Builder) CorruptDeclaredFileSize(v uint32) { b.corruptFileSize = &v }

// TruncateTail 构建后丢弃末尾 n 字节（触发截断类错误）。
func (b *Builder) TruncateTail(n int) { b.dropTail = n }

// FlipByte 构建后翻转指定偏移的一个字节（用于 CRC 损坏）。
func (b *Builder) FlipByte(off int64) { b.flipOffsets = append(b.flipOffsets, off) }

// TLVOffset 返回第 idx 个 TLV 的最终偏移（Build 后有效）。
func (b *Builder) TLVOffset(idx int) int64 { return b.tlvs[idx].offset }

// Build 组装完整 GARC 文件并应用损坏注入。
func (b *Builder) Build() ([]byte, error) {
	tableOff := uint32(garc.HeaderSize)
	mdOff := tableOff + uint32(len(b.tracks))*garc.TrackEntrySize
	var mdLen uint32
	for _, c := range b.tlvs {
		mdLen += uint32(len(c.bytes))
	}
	seiOff := mdOff + mdLen
	var seiLen uint32
	for _, c := range b.seis {
		seiLen += uint32(len(c.bytes))
	}
	total := int64(seiOff) + int64(seiLen)
	if total > garc.MaxFileSize {
		return nil, fmt.Errorf("fixture: file too large")
	}

	out := make([]byte, total)
	// 文件头（CRC 之外的字段先填，最后算 CRC）。
	copy(out[0:4], garc.MagicGARC)
	out[4] = garc.SupportedVersion
	out[5] = b.endianByte
	b.order.PutUint16(out[6:], b.flags)
	b.order.PutUint32(out[8:], uint32(total))
	b.order.PutUint32(out[12:], uint32(len(b.tracks)))
	b.order.PutUint32(out[16:], tableOff)
	b.order.PutUint32(out[20:], mdOff)
	b.order.PutUint32(out[24:], mdLen)
	b.order.PutUint32(out[28:], seiOff)
	b.order.PutUint32(out[32:], seiLen)
	// 36..59 保留零。
	b.order.PutUint32(out[60:], crc32.ChecksumIEEE(out[0:60]))

	// 轨道表。
	for i, t := range b.tracks {
		base := int64(tableOff) + int64(i)*garc.TrackEntrySize
		b.order.PutUint32(out[base:], t.id)
		out[base+4] = t.typ
		out[base+5] = t.hdrFlag
		b.order.PutUint32(out[base+8:], t.seiCount)
	}
	// TLV 区。
	cur := int64(mdOff)
	for i := range b.tlvs {
		b.tlvs[i].offset = cur
		copy(out[cur:], b.tlvs[i].bytes)
		cur += int64(len(b.tlvs[i].bytes))
	}
	// SEI 区。
	cur = int64(seiOff)
	for i := range b.seis {
		b.seis[i].offset = cur
		copy(out[cur:], b.seis[i].bytes)
		cur += int64(len(b.seis[i].bytes))
	}

	// 损坏注入。
	if b.corruptVersion != nil {
		// 版本检查先于头 CRC（见 garc.Parse），无需重算 CRC。
		out[4] = *b.corruptVersion
	}
	if b.corruptFileSize != nil {
		// 篡改声明长度后重算头 CRC，使失败精确落在 LENGTH_MISMATCH。
		b.order.PutUint32(out[8:], *b.corruptFileSize)
		b.order.PutUint32(out[60:], crc32.ChecksumIEEE(out[0:60]))
	}
	for _, off := range b.flipOffsets {
		if off >= 0 && off < int64(len(out)) {
			out[off] ^= 0xFF
		}
	}
	if b.dropTail > 0 && b.dropTail < len(out) {
		out = out[:len(out)-b.dropTail]
	}
	return out, nil
}
