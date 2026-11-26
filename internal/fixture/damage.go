package fixture

import (
	"tsproof/internal/clock"
	"tsproof/internal/ts"
)

// DamageKind 标识一类损坏，用于报表与测试断言。
type DamageKind string

const (
	DamagePCRWrap    DamageKind = "pcr_wrap"
	DamageCCJump     DamageKind = "cc_jump"
	DamageDuplicate  DamageKind = "duplicate"
	DamageReorder    DamageKind = "reorder"
	DamageTruncate   DamageKind = "truncate"
	DamageBadCRCPI   DamageKind = "bad_psi_crc"
	DamagePSIUpdate  DamageKind = "psi_update"
)

// DamageReport 记录每种损坏被注入的次数，供演示输出核对。
type DamageReport struct {
	CCGaps       int `json:"cc_gaps"`
	PSISwitches  int `json:"psi_switches"`
	CRCReject    int `json:"crc_reject"`
	Truncated    int `json:"truncated"`
	Duplicates   int `json:"duplicates"`
	Reorders     int `json:"reorders"`
	PCRWraps     int `json:"pcr_wraps"`
}

// findPayloadPackets 返回指定 PID 上带负载包的全局下标（确定性选取变异点）。
func findPayloadPackets(pkts [][]byte, pid uint16) []int {
	var idxs []int
	for i, raw := range pkts {
		p, err := ts.ParsePacket(raw, i)
		if err != nil || p.PID != pid || !p.HasPayload {
			continue
		}
		idxs = append(idxs, i)
	}
	return idxs
}

// InjectCCJump 在视频 PID 中部把一个包的 CC 加 3，制造连续性缺口。
func InjectCCJump(pkts [][]byte) ([][]byte, int) {
	idxs := findPayloadPackets(pkts, PIDVideo)
	if len(idxs) < 8 {
		return pkts, 0
	}
	target := idxs[len(idxs)/2]
	out := clonePackets(pkts)
	cc := out[target][3] & 0x0F
	out[target][3] = (out[target][3] & 0xF0) | ((cc + 3) & 0x0F)
	return out, 1
}

// InjectDuplicate 复制视频 PID 的一个负载包（相同 CC 紧随其后）。
func InjectDuplicate(pkts [][]byte) ([][]byte, int) {
	idxs := findPayloadPackets(pkts, PIDVideo)
	if len(idxs) < 4 {
		return pkts, 0
	}
	target := idxs[len(idxs)/3]
	out := make([][]byte, 0, len(pkts)+1)
	out = append(out, pkts[:target+1]...)
	dup := make([]byte, ts.PacketSize188)
	copy(dup, pkts[target])
	out = append(out, dup)
	out = append(out, pkts[target+1:]...)
	return out, 1
}

// InjectReorder 交换两个全局相邻的视频负载包（CC 顺序被打乱，
// 且后者的输入序号相对该 PID 上一包倒退，可被乱序检测捕获）。
func InjectReorder(pkts [][]byte) ([][]byte, int) {
	idxs := findPayloadPackets(pkts, PIDVideo)
	if len(idxs) < 6 {
		return pkts, 0
	}
	base := idxs[len(idxs)/4]
	// 找到 base 之后下一个视频负载包的位置。
	next := -1
	for _, j := range idxs {
		if j > base {
			next = j
			break
		}
	}
	if next < 0 {
		return pkts, 0
	}
	out := clonePackets(pkts)
	out[base], out[next] = out[next], out[base]
	return out, 1
}

// InjectTruncate 在流的后段截断，丢弃尾部若干包，制造不完整片段。
func InjectTruncate(pkts [][]byte) ([][]byte, int) {
	cut := len(pkts) * 3 / 4
	if cut >= len(pkts) {
		return pkts, 0
	}
	return clonePackets(pkts)[:cut], len(pkts) - cut
}

// InjectBadCRCPI 定位 PAT 包并破坏其 PSI 节的数据区字节（长度字段保持
// 合法，节仍可装配，但 CRC 必然失配）。
func InjectBadCRCPI(pkts [][]byte) ([][]byte, int) {
	out := clonePackets(pkts)
	for i, raw := range out {
		p, err := ts.ParsePacket(raw, i)
		if err != nil || p.PID != ts.PIDPAT || !p.PUSI {
			continue
		}
		// 包字节 4 是 pointer_field，节头占字节 5..12，数据区从字节 13 起。
		// 改写 program 映射区字节，绕开长度字段且必然改变 CRC 输入。
		pos := 13
		if pos < ts.PacketSize188 && raw[pos] != 0xFF {
			raw[pos] ^= 0xFF
			return out, 1
		}
	}
	return out, 0
}

// InjectPSIUpdate 在流中后段插入版本号 +1 的 PAT/PMT（CRC 正确），
// 映射内容保持一致，触发一次合法 PSI 版本切换。
func InjectPSIUpdate(pkts [][]byte, at int) ([][]byte, int) {
	pat := ts.BuildPAT(TSID, 2,
		[]ts.PATProgram{{ProgramNumber: ProgramNo, PMTPID: PIDPMT}})
	pmt := ts.BuildPMT(ProgramNo, 2, PIDVideo,
		[]ts.PMTStream{
			{StreamType: ts.StreamTypeJPEG, PID: PIDVideo},
			{StreamType: ts.StreamTypePCM, PID: PIDAudio},
		})
	mx := newMuxer()
	mx.writeSection(ts.PIDPAT, pat)
	mx.writeSection(PIDPMT, pmt)
	insert := mx.bytes()
	insertPkts := bytesToPackets(insert)
	if at < 0 || at > len(pkts) {
		at = len(pkts) / 2
	}
	out := make([][]byte, 0, len(pkts)+len(insertPkts))
	out = append(out, pkts[:at]...)
	out = append(out, insertPkts...)
	out = append(out, pkts[at:]...)
	return out, len(insertPkts)
}

// InjectPCRWrap 把后段视频 PCR 改写为跨越 2^33 边界的序列：
// 前几个采样在模数高位区，随后回绕到低位区继续递增，制造真实回绕边沿。
func InjectPCRWrap(pkts [][]byte) ([][]byte, int) {
	out := clonePackets(pkts)
	hits := 0
	mod := int64(clock.PCRModulus)
	startHigh := mod - 2000 // 高位区起点
	for i := len(out) / 2; i < len(out); i++ {
		p, err := ts.ParsePacket(out[i], i)
		if err != nil || !p.HasPCR() {
			continue
		}
		// 线性递增后对模数取模：第 3 个采样起越过边界回到低位区。
		v := (startHigh + int64(hits)*1500) % mod
		ts.EncodePCR(out[i], 5, v)
		hits++
	}
	if hits == 0 {
		return pkts, 0
	}
	return out, hits
}

func clonePackets(pkts [][]byte) [][]byte {
	out := make([][]byte, len(pkts))
	for i, p := range pkts {
		c := make([]byte, len(p))
		copy(c, p)
		out[i] = c
	}
	return out
}

func bytesToPackets(buf []byte) [][]byte {
	n := len(buf) / ts.PacketSize188
	out := make([][]byte, n)
	for i := 0; i < n; i++ {
		out[i] = buf[i*ts.PacketSize188 : (i+1)*ts.PacketSize188]
	}
	return out
}
