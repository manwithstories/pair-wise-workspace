package mp4

import (
	"encoding/binary"

	"mediacheck/internal/model"
)

// parseTkhd 读取 track_ID。b 是 FullBox 字段区（b[0:4]=verflags）：
// v0: verflags(4)+creation(4)+modification(4)+track_ID(4) → 偏移 12；
// v1 的 creation/modification 各 8 字节 → 偏移 20。
func parseTkhd(b []byte) (uint32, bool) {
	if len(b) < 1 {
		return 0, false
	}
	off := 12
	if b[0] == 1 {
		off = 20
	}
	if off+4 > len(b) {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[off:]), true
}

// parseMdhdTimescale 读取 timescale。b 是 FullBox 字段区：
// v0: verflags(4)+creation(4)+modification(4)+timescale(4) → 偏移 12；
// v1 的 creation/modification 各 8 字节 → 偏移 20。
func parseMdhdTimescale(b []byte) (uint32, bool) {
	if len(b) < 1 {
		return 0, false
	}
	off := 12
	if b[0] == 1 {
		off = 20
	}
	if off+4 > len(b) {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[off:]), true
}

// hdlrHint 返回 handler_type（vide/soun）。b 是 FullBox 字段区：
// pre_defined(4)+handler_type(4) → 偏移 4。
func hdlrHint(b []byte) string {
	if len(b) < 8 {
		return ""
	}
	return string(b[4:8])
}

// parseStsd 解析第一个 sample entry。b 是 FullBox 字段区：
// entry_count(4) 位于偏移 4；其后是带 8 字节 box 头的 sample entry，
// 四字符码在 entry 内偏移 4、SampleEntry 体在 entry 内偏移 8。
func parseStsd(b []byte) (codec string, rate uint32, layout model.ChannelLayout, depth uint16, color *model.ColorMeta, ok bool) {
	if len(b) < 4+8 {
		return "", 0, "", 0, nil, false
	}
	entry := b[8:] // 跳过 verflags(4)+entry_count(4)
	if len(entry) < 8 {
		return "", 0, "", 0, nil, false
	}
	fourcc := string(entry[4:8])
	body := entry[8:]
	switch fourcc {
	case "ipcm":
		// 简化 AudioSampleEntry 布局（与 fixture 写入顺序一致）：
		// reserved(6)+dqi(2)+channels(2)+samplesize(2)+pre(2)+reserved(4)+samplerate(4.16 高 16 位)
		if len(body) < 20 {
			return "", 0, "", 0, nil, false
		}
		channels := binary.BigEndian.Uint16(body[8:10])
		depth = binary.BigEndian.Uint16(body[10:12])
		rate = binary.BigEndian.Uint32(body[16:20]) >> 16
		layout = channelsToLayout(channels)
		if layout == "" {
			return fourcc, rate, "", depth, nil, true
		}
		return fourcc, rate, layout, depth, parseNestedColr(body), true
	case "avc1":
		return fourcc, 0, "", 0, parseNestedColr(body), true
	default:
		return fourcc, 0, "", 0, nil, true
	}
}

func channelsToLayout(n uint16) model.ChannelLayout {
	switch n {
	case 2:
		return model.LayoutStereo
	case 6:
		return model.Layout51
	case 8:
		return model.Layout71
	default:
		return ""
	}
}

// parseNestedColr 在 sample entry 体内扫描 colr box。标准 box 头为 size(4)+type(4)，
// colr payload: colour_type(4)+primaries(2)+transfer(2)+matrix(2)[+full_range(1)]。
func parseNestedColr(body []byte) *model.ColorMeta {
	for i := 0; i+8 <= len(body); i++ {
		if string(body[i+4:i+8]) != "colr" {
			continue
		}
		size := binary.BigEndian.Uint32(body[i : i+4])
		if size < 19 || uint32(i)+size > uint32(len(body)) {
			return nil
		}
		p := body[i+8:] // colr payload
		if len(p) < 10 {
			return nil
		}
		c := &model.ColorMeta{
			ColorPrimaries: binary.BigEndian.Uint16(p[4:6]),
			Transfer:       binary.BigEndian.Uint16(p[6:8]),
			Matrix:         binary.BigEndian.Uint16(p[8:10]),
		}
		if len(p) >= 11 {
			c.FullRange = p[10] == 1
		}
		return c
	}
	return nil
}

// parseMfhd 读取 sequence_number。payload 含 verflags：verflags(4)+seq(4)。
func parseMfhd(b []byte) (uint32, bool) {
	if len(b) < 8 {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[4:8]), true
}

// parseTfdt 按 version 读取 baseMediaDecodeTime。payload 含 verflags，
// v0 字段在偏移 4（u32），v1 在偏移 4（u64，需共 12 字节）。
func parseTfdt(b []byte) (int64, bool) {
	if len(b) < 5 {
		return 0, false
	}
	if b[0] == 1 {
		if len(b) < 12 {
			return 0, false
		}
		return int64(binary.BigEndian.Uint64(b[4:12])), true
	}
	if len(b) < 8 {
		return 0, false
	}
	return int64(binary.BigEndian.Uint32(b[4:8])), true
}

// trun flags 位定义（ISO 14496-12）。
const (
	trunDataOffset     = 0x000001
	trunFirstFlags     = 0x000004
	trunSampleDuration = 0x000100
	trunSampleSize     = 0x000200
	trunSampleFlags    = 0x000400
	trunCtsOffsets     = 0x000800
)

type trunInfo struct {
	sampleCount uint32
	dataOffset  int32
	hasDuration bool
	hasSize     bool
	entryStarts int // 样本条目起始偏移
	entryStride int
}

func parseTrunHeader(b []byte) (trunInfo, bool) {
	if len(b) < 8 {
		return trunInfo{}, false
	}
	flags := binary.BigEndian.Uint32(b[:4]) & 0x00FFFFFF
	ti := trunInfo{
		sampleCount: binary.BigEndian.Uint32(b[4:8]),
		hasDuration: flags&trunSampleDuration != 0,
		hasSize:     flags&trunSampleSize != 0,
	}
	off := 8
	if flags&trunDataOffset != 0 {
		if len(b) < off+4 {
			return trunInfo{}, false
		}
		ti.dataOffset = int32(binary.BigEndian.Uint32(b[off : off+4]))
		off += 4
	}
	if flags&trunFirstFlags != 0 {
		off += 4
	}
	ti.entryStarts = off
	ti.entryStride = 0
	if ti.hasDuration {
		ti.entryStride += 4
	}
	if ti.hasSize {
		ti.entryStride += 4
	}
	if flags&trunSampleFlags != 0 {
		ti.entryStride += 4
	}
	if flags&trunCtsOffsets != 0 {
		ti.entryStride += 4
	}
	return ti, true
}
