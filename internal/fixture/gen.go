// Package fixture 按种子确定性生成合成 MPEG-TS 流、RTP 音频馈送与色彩输入，
// 并按需注入 PCR 回绕、连续计数异常、PSI 版本切换、截断表、采样率切换与色彩冲突。
package fixture

import (
	"fmt"
	"math/rand"
	"strings"

	"github.com/pion/rtp"

	"mediatool/internal/clock"
	"mediatool/internal/ts"
	"mediatool/internal/video"
)

// FaultSpec 选择要注入的故障。
type FaultSpec struct {
	PCRWrap, CCGap, CCConflict, PSIVersion, TruncatedPSI, AudioRate, ColorConflict bool
}

// ParseFaultList 解析逗号分隔的故障名列表。
func ParseFaultList(s string) (FaultSpec, error) {
	var f FaultSpec
	for _, name := range strings.Split(s, ",") {
		switch strings.TrimSpace(name) {
		case "":
		case "pcr-wrap":
			f.PCRWrap = true
		case "cc-gap":
			f.CCGap = true
		case "cc-conflict":
			f.CCConflict = true
		case "psi-version":
			f.PSIVersion = true
		case "truncated-psi":
			f.TruncatedPSI = true
		case "audio-rate":
			f.AudioRate = true
		case "color-conflict":
			f.ColorConflict = true
		default:
			return f, fmt.Errorf("unknown fault %q", name)
		}
	}
	return f, nil
}

// Stream 是一次生成的全部输入。
type Stream struct {
	Data        []byte   // TS 字节流
	RTP         [][]byte // 独立音频设备的 RTP 包
	ColorInputs []video.Input
	DurationMs  int
}

const (
	PIDPMT   uint16 = 0x100
	PIDVideo uint16 = 0x101
	PIDAudio uint16 = 0x102
)

// EncodePTS 按 MPEG PES 格式编码 33 位 PTS。
func EncodePTS(pts uint64) []byte {
	return []byte{
		0x21 | byte((pts>>29)&0x0E),
		byte(pts >> 22),
		byte((pts>>14)&0xFE) | 1,
		byte(pts >> 7),
		byte((pts<<1)&0xFE) | 1,
	}
}

func section(tableID byte, version int, body []byte) []byte {
	secLen := 5 + len(body) + 4
	raw := []byte{tableID, 0xB0 | byte(secLen>>8), byte(secLen),
		0x00, 0x01, 0xC1 | byte(version<<1), 0x00, 0x00}
	raw = append(raw, body...)
	crc := ts.CRC32MPEG(raw)
	return append(raw, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

func pmtBody(version int, padTo int) []byte {
	body := []byte{0xE0 | byte(PIDVideo>>8), byte(PIDVideo & 0xFF), 0xF0, 0x00}
	body = append(body, 0x1B, 0xE0|byte(PIDVideo>>8), byte(PIDVideo&0xFF), 0xF0, 0x00)
	body = append(body, 0x0F, 0xE0|byte(PIDAudio>>8), byte(PIDAudio&0xFF), 0xF0, 0x00)
	if version >= 1 {
		body = append(body, 0x06, 0xE1, 0x03, 0xF0, 0x00) // 新增数据流
	}
	for len(body) < padTo {
		body = append(body, 0xFF)
	}
	return body
}

// packetize 把负载切成 TS 包；首包可携带 PCR 与 payload_start。
// 返回包列表；cc 按 PID 由调用方维护。
func packetize(pid uint16, cc *uint8, payloadStart bool, pcr *int64, payload []byte) [][]byte {
	var pkts [][]byte
	first := true
	for len(payload) > 0 || first {
		pkt := make([]byte, ts.PacketSize)
		pkt[0] = ts.SyncByte
		pkt[1] = byte(pid >> 8)
		pkt[2] = byte(pid)
		pos := 4
		if pcr != nil && first {
			pkt[3] = 0x30 | *cc&0x0F
			adaptLen := ts.PacketSize - 4 - 1
			if len(payload) < ts.PacketSize-4 {
				adaptLen = ts.PacketSize - 4 - len(payload) - 1
			}
			pkt[4] = byte(adaptLen)
			pkt[5] = 0x10
			v := uint64(*pcr) & uint64(clock.Mod33-1)
			pkt[6] = byte(v >> 25)
			pkt[7] = byte(v >> 17)
			pkt[8] = byte(v >> 9)
			pkt[9] = byte(v >> 1)
			pkt[10] = byte(v<<7) | 0x7E
			pkt[11] = 0
			for i := 12; i < 5+adaptLen; i++ {
				pkt[i] = 0xFF
			}
			pos = 5 + adaptLen
		} else {
			pkt[3] = 0x10 | *cc&0x0F
		}
		if first && payloadStart {
			pkt[1] |= 0x40
			if pcr == nil {
				pkt[4] = 0x00 // PSI pointer field
				pos = 5
			}
		}
		n := copy(pkt[pos:], payload)
		payload = payload[n:]
		for i := pos + n; i < ts.PacketSize; i++ {
			pkt[i] = 0xFF
		}
		pkts = append(pkts, pkt)
		*cc = (*cc + 1) & 0x0F
		first = false
		if pcr == nil && !payloadStart {
			break // PES 负载单包足够
		}
	}
	return pkts
}

// Generate 生成全部输入。durationMs 为时长，seed 决定伪随机负载内容。
func Generate(seed int64, durationMs int, f FaultSpec) Stream {
	rng := rand.New(rand.NewSource(seed))
	s := Stream{DurationMs: durationMs}
	var out []byte
	ccPAT, ccPMT, ccVideo, ccAudio := uint8(0), uint8(0), uint8(0), uint8(0)

	emit := func(pkts [][]byte) {
		for _, p := range pkts {
			out = append(out, p...)
		}
	}
	emitPSI := func(version int, padTo int, dropTail bool) {
		pat := section(0x00, 0, []byte{0x00, 0x01, 0xE0 | byte(PIDPMT>>8), byte(PIDPMT & 0xFF)})
		emit(packetize(0, &ccPAT, true, nil, pat))
		pkts := packetize(PIDPMT, &ccPMT, true, nil, section(0x02, version, pmtBody(version, padTo)))
		if dropTail && len(pkts) > 1 {
			pkts = pkts[:len(pkts)-1] // 截断：末包丢失
		}
		emit(pkts)
	}

	pcrBase := int64(0)
	if f.PCRWrap {
		pcrBase = clock.Mod33 - 3*clock.ClockRate // 3 秒后回绕
	}
	frames := durationMs / 40
	dropAt, conflictAt := frames/3, frames/2
	emitPSI(0, 0, false)
	for i := 0; i < frames; i++ {
		if f.PSIVersion && i == frames/2 {
			emitPSI(1, 0, false)
		}
		if f.TruncatedPSI && i == frames/2 {
			emitPSI(0, 300, true) // 跨包大表被截断
		}
		pts := uint64(pcrBase + int64(i)*3600)
		pcr := pcrBase + int64(i)*3600
		pes := append([]byte{0x00, 0x00, 0x01, 0xE0, 0x00, 0x00, 0x80, 0x80, 0x05},
			EncodePTS(pts)...)
		payload := append(pes, make([]byte, 120)...)
		rng.Read(payload[len(pes):])
		if f.CCGap && i == dropAt {
			ccVideo++ // 丢一包：CC 前进而包未发出
			continue
		}
		if f.CCConflict && i == conflictAt {
			saved := ccVideo
			ccVideo = (ccVideo + 9) & 0x0F // 不可恢复跳变
			emit(packetize(PIDVideo, &ccVideo, true, &pcr, payload))
			ccVideo = saved
			continue
		}
		emit(packetize(PIDVideo, &ccVideo, true, &pcr, payload))
		if i%2 == 0 { // 音频每 80ms 一包
			apts := uint64(pcrBase + int64(i)*3600)
			apes := append([]byte{0x00, 0x00, 0x01, 0xC0, 0x00, 0x00, 0x80, 0x80, 0x05},
				EncodePTS(apts)...)
			apayload := append(apes, make([]byte, 140)...)
			rng.Read(apayload[len(apes):])
			emit(packetize(PIDAudio, &ccAudio, true, nil, apayload))
		}
	}
	s.Data = out
	s.RTP = genRTP(rng, durationMs, f.AudioRate)
	s.ColorInputs = genColor(f.ColorConflict)
	return s
}

func genRTP(rng *rand.Rand, durationMs int, rateSwitch bool) [][]byte {
	var out [][]byte
	pt, rate := uint8(96), 48000
	n := durationMs / 20
	for i := 0; i < n; i++ {
		if rateSwitch && i == n/2 {
			pt, rate = 97, 44100
		}
		payload := make([]byte, 96)
		rng.Read(payload)
		p := rtp.Packet{Header: rtp.Header{
			Version: 2, PayloadType: pt,
			SequenceNumber: uint16(1000 + i),
			Timestamp:      uint32(i * rate / 50),
			SSRC:           0xC0FFEE,
		}, Payload: payload}
		raw, err := p.Marshal()
		if err != nil {
			panic(err)
		}
		out = append(out, raw)
	}
	return out
}

func genColor(conflict bool) []video.Input {
	m709 := video.Metadata{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", Range: "limited"}
	if !conflict {
		return []video.Input{
			{Source: "cam-a", Priority: 0, Meta: m709},
			{Source: "cam-b", Priority: 1, Meta: m709},
		}
	}
	return []video.Input{
		{Source: "cam-a", Priority: 0, Meta: m709},
		{Source: "cam-b", Priority: 1, Meta: video.Metadata{
			Primaries: "bt2020", Transfer: "pq", Matrix: "bt2020nc", Range: "full"}},
	}
}
