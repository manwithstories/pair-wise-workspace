package stream

import "bytes"

// newByteReader 把内存字节包装为 io.Reader，splitUnits 仅依赖 Read。
func newByteReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// classify 检查 AU 内全部语法单元，返回关键帧标记、首个发现的错误及其
// 出错单元的精确字节偏移。错误不会中断扫描：后续单元继续解析，保证一次
// 运行给出完整诊断面。
func (s *Scanner) classify(b []byte, units []unit, ar auRange) (keyframe bool, kind IssueKind, msg string, errOff int64) {
	keyframe = true // 无 VCL/帧单元时保持保守，由调用侧按实际帧修正
	sawFrame := false
	errOff = -1
	for i := ar.lo; i < ar.hi; i++ {
		u := units[i]
		payload := b[u.data:u.end]
		if len(payload) == 0 {
			if kind == "" {
				kind, msg, errOff = IssueEmptyUnit, "零长度语法单元", u.start
			}
			continue
		}
		var kf bool
		var k IssueKind
		var m string
		switch s.codec {
		case H264:
			kf, k, m = classifyH264NAL(payload)
			if isH264VCLType(payload[0] & 0x1F) {
				sawFrame = true
				keyframe = keyframe && kf
			}
		case H265:
			kf, k, m = classifyH265NAL(payload)
			if hevcNALType(payload) <= 31 {
				sawFrame = true
				keyframe = keyframe && kf
			}
		case AV1:
			kf, k, m = classifyAV1OBU(payload)
			if isAV1FrameOBU(payload) {
				sawFrame = true
				keyframe = kf
			}
		}
		if k != "" && kind == "" {
			kind, msg, errOff = k, m, u.start
		}
	}
	if !sawFrame {
		// 仅含参数集/元数据的 AU 不构成可独立解码帧。
		keyframe = false
	}
	return keyframe, kind, msg, errOff
}

// --- H.264 (ISO/IEC 14496-10 §7.3.1) ---

func isH264VCLType(t byte) bool { return t >= 1 && t <= 5 }

func isVCL(codec Codec, payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	switch codec {
	case H264:
		return isH264VCLType(payload[0] & 0x1F)
	case H265:
		// H.265 NAL 头占 2 字节，长度不足无法判定为 VCL。
		return len(payload) >= 2 && hevcNALType(payload) <= 31
	default:
		return false
	}
}

func classifyH264NAL(p []byte) (keyframe bool, kind IssueKind, msg string) {
	h := p[0]
	if h&0x80 != 0 { // forbidden_zero_bit
		return false, IssueForbiddenNAL, "H.264 NAL forbidden_zero_bit 被置 1"
	}
	t := h & 0x1F
	if t == 0 || (t >= 13 && t <= 18) || t >= 25 {
		return false, IssueIllegalOBU, "H.264 保留/未定义 NAL 类型"
	}
	// IDR(5) 与新引入的 CRA(21) 为随机访问点。
	return t == 5 || t == 21, "", ""
}

// --- H.265 (ISO/IEC 23008-2 §7.3.1.2) ---

func hevcNALType(p []byte) uint8 {
	if len(p) < 2 {
		return 0xFF
	}
	return (p[0] >> 1) & 0x3F
}

func classifyH265NAL(p []byte) (keyframe bool, kind IssueKind, msg string) {
	h := p[0]
	if h&0x80 != 0 { // forbidden_zero_bit
		return false, IssueForbiddenNAL, "H.265 NAL forbidden_zero_bit 被置 1"
	}
	t := hevcNALType(p)
	switch t {
	case 19, 20: // IDR_W_RADL / IDR_N_LP
		return true, "", ""
	case 21: // CRA
		return true, "", ""
	default:
		return false, "", ""
	}
}

// --- AV1 低开销码流 (AV1 Bitstream §6) ---

// OBU 类型常量（AV1 §6.2.2）。
const (
	obuSequenceHeader = 1
	obuFrame          = 6
	obuFrameHeader    = 3
	obuMetadata       = 5
	obuTileGroup      = 4
)

// isAV1FrameOBU 判断 OBU 是否携带帧头（FRAME / FRAME_HEADER）。
func isAV1FrameOBU(p []byte) bool {
	if len(p) == 0 {
		return false
	}
	t := (p[0] >> 3) & 0x0F
	return t == obuFrame || t == obuFrameHeader
}

func classifyAV1OBU(p []byte) (keyframe bool, kind IssueKind, msg string) {
	h := p[0]
	if h&0x80 != 0 { // obu_forbidden_bit
		return false, IssueIllegalOBU, "AV1 OBU forbidden 位被置 1"
	}
	t := (h >> 3) & 0x0F
	if t == 0 || t == 2 || t == 7 || t == 15 {
		return false, IssueIllegalOBU, "AV1 保留 OBU 类型"
	}
	hasSize := h&0x02 != 0
	if !hasSize {
		return false, IssueIllegalOBU, "低开销码流要求 obu_has_size_field=1"
	}
	if len(p) < 2 {
		return false, IssueTruncated, "AV1 OBU 缺少 leb128 长度字段"
	}
	// 解析 leb128 长度并校验不越界。
	_, n := readLEB128(p[1:])
	if n == 0 {
		return false, IssueTruncated, "AV1 OBU leb128 长度被截断"
	}
	if !isAV1FrameOBU(p) {
		return false, "", ""
	}
	// 帧 OBU 必须在 leb128 头之后还有净荷（起始码成帧已保证单元完整，
	// 这里只需确认头之后确实存在帧头字节）。
	if len(p) <= 1+n {
		return false, IssueTruncated, "AV1 帧 OBU 净荷为空"
	}
	// 合成帧头（fixture 约定）：净荷首字节 bit7 即 keyframe 指示。
	payload := p[1+n:]
	return payload[0]&0x80 != 0, "", ""
}

// readLEB128 解析无符号 LEB128，返回值与占用字节数；非法时 n=0。
func readLEB128(b []byte) (value uint64, n int) {
	var shift uint
	for i, c := range b {
		if i >= 8 {
			return 0, 0
		}
		value |= uint64(c&0x7F) << shift
		n = i + 1
		if c&0x80 == 0 {
			return value, n
		}
		shift += 7
	}
	return 0, 0
}
