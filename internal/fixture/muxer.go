package fixture

import (
	"tsproof/internal/ts"
)

// 演示流固定 PID 布局。
const (
	PIDVideo  uint16 = 0x0100 // JPEG 帧（同时承载 PCR）
	PIDAudio  uint16 = 0x0101 // PCM 音频
	PIDPMT    uint16 = 0x1000
	ProgramNo uint16 = 0x0001
	TSID      uint16 = 0x04D2
)

// muxer 把节与 PES 负载复用成 188 字节 TS 包，逐 PID 维护 CC，
// 并在视频 PID 上周期插入 PCR。所有输出都是确定性的。
type muxer struct {
	out      [][]byte
	cc       map[uint16]uint8
	order    int
	pcrEvery int // 每隔多少个视频包写一次 PCR
	videoCnt int
}

func newMuxer() *muxer {
	return &muxer{cc: map[uint16]uint8{}, pcrEvery: 3}
}

// bytes 拼接全部包，返回连续 TS 字节流。
func (m *muxer) bytes() []byte {
	total := len(m.out) * ts.PacketSize188
	buf := make([]byte, 0, total)
	for _, p := range m.out {
		buf = append(buf, p...)
	}
	return buf
}

// writeSection 把完整 PSI 节作为一个 PID 的负载发出（首包带 pointer=0）。
func (m *muxer) writeSection(pid uint16, section []byte) {
	payload := append([]byte{0x00}, section...)
	m.writePESPayload(pid, payload, true)
}

// writePESPayload 把任意负载按 184 字节切片复用，首片 PUSI=true。
func (m *muxer) writePESPayload(pid uint16, payload []byte, pusi bool) {
	for len(payload) > 0 {
		n := len(payload)
		if n > 184 {
			n = 184
		}
		m.emit(pid, pusi, payload[:n], -1)
		payload = payload[n:]
		pusi = false
	}
}

// emit 产出单个 TS 包。pcr27 < 0 表示不带 PCR。
func (m *muxer) emit(pid uint16, pusi bool, payload []byte, pcr27 int64) {
	raw := make([]byte, ts.PacketSize188)
	raw[0] = ts.SyncByte
	if pusi {
		raw[1] |= 0x40
	}
	raw[1] |= byte(pid >> 8)
	raw[2] = byte(pid)
	cc := m.cc[pid]
	needPCR := pcr27 >= 0
	var afc byte = 0x01 // payload only
	pos := 4
	if needPCR {
		afc = 0x03 // adaptation + payload
		// adaptation: length, flags(PCR=0x10), 6 bytes PCR
		raw[4] = 7
		raw[5] = 0x10
		ts.EncodePCR(raw, 5, pcr27)
		pos = 4 + 1 + 7
	}
	raw[3] = (afc << 4) | (cc & 0x0F)
	space := ts.PacketSize188 - pos
	n := len(payload)
	if n > space {
		n = space
	}
	// 未使用的负载字节填 0xFF（标准 TS stuffing），供 PSI 装配器识别填充。
	for i := pos; i < ts.PacketSize188; i++ {
		raw[i] = 0xFF
	}
	copy(raw[pos:], payload[:n])
	m.out = append(m.out, raw)
	m.cc[pid] = (cc + 1) & 0x0F
	m.order++
	if pid == PIDVideo {
		m.videoCnt++
	}
}

// maybePCR 按节奏在视频 PID 上发出一个仅适配域 PCR 包。
func (m *muxer) maybePCR(pcr27 int64) {
	if m.videoCnt == 0 || m.videoCnt%m.pcrEvery != 0 {
		return
	}
	raw := make([]byte, ts.PacketSize188)
	raw[0] = ts.SyncByte
	raw[1] = byte((PIDVideo >> 8) & 0x1F)
	raw[2] = byte(PIDVideo & 0xFF)
	raw[4] = 7
	raw[5] = 0x10
	ts.EncodePCR(raw, 5, pcr27)
	// adaptation-only: afc=0b10。本项目解析侧 CCTracker 只对带负载包
	// 计数，因此适配域-only 包保持同一 CC，不制造媒体负载缺口。
	raw[3] = 0x20 | (m.cc[PIDVideo] & 0x0F)
	m.out = append(m.out, raw)
	m.order++
	m.videoCnt++
}
