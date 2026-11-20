// Package testdata 自生成确定性样例与故障夹具：
// 正常、截断、乱序和边界输入。相同 seed 必产生相同输出。
package testdata

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"sort"
	"strings"

	"github.com/pion/rtp"

	"mediamux/media"
	"mediamux/transport"
)

// 演示流参数。
const (
	ClockRate   = 9000 * 10 // RTP 时钟 90 kHz
	FrameMs     = 40        // 25fps
	PacketBytes = 6000      // 单包负载（本地分帧，无 MTU 限制）
	MaxPacket   = 8192      // 单包字节上限
	MaxRecord   = 4096      // 单条字幕字节上限
)

// Stream 是一次生成的 RTP 流及其对应的每帧字节数。
type Stream struct {
	Packets    []*transport.Packet // 按到达时间排序，故障已注入
	FrameSizes []uint32            // 每帧负载字节数（供 MP4 样本表）
	Frames     int
}

// RTPStream 生成按目标码率步调的 RTP 流，注入确定性故障：
// 突发丢包、相邻乱序、重复包与轻微时钟漂移（5ms/s）。
// 丢包区域避开字幕 cue 边界，保证对齐统计可解释。
func RTPStream(seed int64, durationMs int, targetBps int64) *Stream {
	r := rand.New(rand.NewSource(seed))
	frames := durationMs / FrameMs
	perFrame := int(targetBps/8) * FrameMs / 1000
	// 突发丢包：5-8 个连续包，避开 50 帧（2000ms）的 cue 边界
	lossFrame := frames/3 + r.Intn(frames/5)
	for (lossFrame%50) < 6 || (lossFrame%50) > 44 {
		lossFrame++
	}
	lossLen := 5 + r.Intn(4)

	type item struct {
		pkt *transport.Packet
		arr media.Ticks
	}
	var items []item
	var frameSizes []uint32
	seq := uint16(0)
	dropped := 0
	for f := 0; f < frames; f++ {
		mediaMs := int64(f * FrameMs)
		size := perFrame
		frameSizes = append(frameSizes, uint32(size))
		for size > 0 {
			n := size
			if n > PacketBytes {
				n = PacketBytes
			}
			size -= n
			if f == lossFrame && dropped < lossLen {
				dropped++
				seq++
				continue // 突发丢包
			}
			payload := make([]byte, n)
			r.Read(payload)
			ts := uint32(f * FrameMs * ClockRate / 1000)
			jit := int64(r.Intn(41) - 20) // ±20ms 抖动
			drift := mediaMs / 200        // 5ms/s 时钟漂移
			arr := media.MillisToTicks(mediaMs + 50 + jit + drift)
			items = append(items, item{
				pkt: &transport.Packet{
					Seq: seq, Timestamp: ts, SSRC: 0x12776,
					Payload: payload, Arrival: arr,
				},
				arr: arr,
			})
			seq++
		}
	}
	// 相邻乱序：交换 3 对
	for i := 0; i < 3; i++ {
		j := 1 + r.Intn(len(items)-2)
		items[j], items[j+1] = items[j+1], items[j]
	}
	// 重复包：复制 2 个，延迟 7ms 再次到达
	for i := 0; i < 2; i++ {
		j := r.Intn(len(items))
		dup := *items[j].pkt
		dup.Arrival = items[j].arr + media.MillisToTicks(7)
		items = append(items, item{pkt: &dup, arr: dup.Arrival})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].arr < items[j].arr })
	st := &Stream{Frames: frames, FrameSizes: frameSizes}
	for _, it := range items {
		st.Packets = append(st.Packets, it.pkt)
	}
	return st
}

// SRT 生成与媒体时间线对齐的正常 SRT 字幕（开始时间带 ±12ms 抖动）。
func SRT(seed int64, durationMs int) []byte {
	r := rand.New(rand.NewSource(seed + 1))
	var b bytes.Buffer
	for i := 0; i*2000+1600 < durationMs; i++ {
		start := int64(i*2000) + int64(r.Intn(25) - 12)
		end := start + 1600
		fmt.Fprintf(&b, "%d\n%s --> %s\n字幕事件 %d（seed %d）\n\n",
			i+1, srtTS(start), srtTS(end), i+1, seed)
	}
	return b.Bytes()
}

func srtTS(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d,%03d",
		ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// Fixture 是一个故障夹具。
type Fixture struct {
	Name   string
	Kind   string // "srt" | "vtt" | "rtp"
	Data   []byte
}

// Fixtures 返回故障夹具：截断字幕、非法时间码、超长记录与损坏 RTP 流。
// 每个夹具在坏记录之后都带有有效记录，用于验证恢复边界。
func Fixtures() []Fixture {
	truncated := "1\n00:00:01,000 --> 00:00:02,000\n完整记录\n\n" +
		"2\n00:00:03,000 --> 00:0" // 时间轴被切断，且末尾无换行
	badTimecode := "1\n00:00:61,000 --> 00:00:02,000\n非法时间码\n\n" +
		"2\n00:00:03,000 --> 00:00:04,000\n仍有效\n"
	overlong := "1\n00:00:01,000 --> 00:00:02,000\n" +
		strings.Repeat("长", 2000) + "\n\n" +
		"2\n00:00:03,000 --> 00:00:04,000\n仍有效\n"
	badVTT := "WEBVTT\n\n" +
		"00:00:01.500 --> 00:00:02.000\n毫秒位非法\n\n" +
		"00:00:03.000 --> 00:00:04.000\n仍有效\n"
	return []Fixture{
		{Name: "truncated_srt", Kind: "srt", Data: []byte(truncated)},
		{Name: "bad_timecode_srt", Kind: "srt", Data: []byte(badTimecode)},
		{Name: "overlong_srt", Kind: "srt", Data: []byte(overlong)},
		{Name: "bad_clock_vtt", Kind: "vtt", Data: []byte(badVTT)},
		{Name: "corrupt_rtp", Kind: "rtp", Data: corruptRTP()},
	}
}

// corruptRTP 生成损坏 RTP 流：好包 + 越界长度记录 + 好包 + 截断尾部。
func corruptRTP() []byte {
	pkt := func(seq uint16) []byte {
		p := rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 3000, SSRC: 9},
			Payload: []byte("demo"),
		}
		b, err := p.Marshal()
		if err != nil {
			panic(err)
		}
		var out bytes.Buffer
		var hdr [2]byte
		binary.BigEndian.PutUint16(hdr[:], uint16(len(b)))
		out.Write(hdr[:])
		out.Write(b)
		return out.Bytes()
	}
	var buf bytes.Buffer
	buf.Write(pkt(1))
	buf.Write([]byte{0x00, 0x05, 1, 2, 3, 4, 5}) // 长度 5 < 12，可恢复跳过
	buf.Write(pkt(2))
	buf.Write([]byte{0x00, 0x40, 1, 2}) // 包体截断，不可恢复
	return buf.Bytes()
}
