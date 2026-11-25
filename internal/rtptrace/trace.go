// Package rtptrace 解析带长度前缀的 RTP 诊断记录文件。
//
// 记录格式（全大端、无外部依赖）：
//
//	uint32 length | RTP packet bytes | uint32 length | RTP packet bytes | ...
//
// 解析在固定的包数与内存预算内进行；乱序、16 位序号回绕、丢包与时间戳跳变
// 统一登记为 model.Fault，并保留扩展 64 位序号以正确跨越回绕点。
package rtptrace

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/pion/rtp"

	"relaycheck/internal/model"
)

// maxRecordBytes 限制单条 RTP 记录的最大长度，防止畸形长度字段耗尽内存。
const maxRecordBytes = 65535

// ErrTraceTruncated 表示长度前缀声明的数据在文件结束前缺失。
var ErrTraceTruncated = errors.New("rtp trace truncated")

// Packet 是一条解析后的 RTP 记录（仅保留诊断所需字段）。
type Packet struct {
	Record int     // 文件内记录索引，从 0 起
	Seq16  uint16  // 原始 16 位序号
	ExtSeq uint64  // 扩展 64 位序号（含回绕次数）
	RTPTS  uint32  // RTP 时间戳（时钟单位）
	SSRC   uint32
}

// Trace 是一次解析的完整结果。
type Trace struct {
	Packets []Packet
	Faults  []model.Fault
	Segment int
}

// ParseFile 流式解析 RTP 诊断文件，segment 用于把故障定位到分片。
// mem 约束读缓冲与记录留存；maxPackets>0 时限制包数硬边界。
func ParseFile(path string, segment int, mem *model.MemTracker, maxPackets int) (Trace, error) {
	f, err := os.Open(path)
	if err != nil {
		return Trace{}, err
	}
	defer f.Close()
	return Parse(f, segment, mem, maxPackets)
}

// Parse 从 r 解析记录流。
func Parse(r io.Reader, segment int, mem *model.MemTracker, maxPackets int) (Trace, error) {
	tr := Trace{Segment: segment}
	br := bufio.NewReaderSize(r, 1<<16)
	var lenBuf [4]byte
	record := 0
	var lastExt uint64
	haveLast := false
	var lastTS uint32
	haveTS := false

	for {
		if _, err := io.ReadFull(br, lenBuf[:]); err != nil {
			if errors.Is(err, io.EOF) {
				break // 正常结束
			}
			return tr, err
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		if n == 0 || n > maxRecordBytes {
			return tr, fmt.Errorf("record %d: invalid length %d", record, n)
		}
		if maxPackets > 0 && record >= maxPackets {
			return tr, fmt.Errorf("record %d: packet count limit %d exceeded", record, maxPackets)
		}
		if mem != nil {
			if err := mem.Acquire(int64(n)); err != nil {
				return tr, err
			}
		}
		buf := make([]byte, n)
		got, err := io.ReadFull(br, buf)
		if err != nil {
			return tr, fmt.Errorf("%w at record %d: need %d bytes, got %d",
				ErrTraceTruncated, record, n, got)
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf); err != nil {
			return tr, fmt.Errorf("record %d: bad rtp packet: %w", record, err)
		}
		ext := extendSeq(lastExt, haveLast, pkt.SequenceNumber)
		tr.Packets = append(tr.Packets, Packet{
			Record: record, Seq16: pkt.SequenceNumber,
			ExtSeq: ext, RTPTS: pkt.Timestamp, SSRC: pkt.SSRC,
		})
		tr.detect(segment, record, ext, pkt.Timestamp, lastExt, haveLast, lastTS, haveTS)
		lastExt = ext
		haveLast = true
		lastTS = pkt.Timestamp
		haveTS = true
		if mem != nil {
			mem.Release(int64(n))
		}
		record++
	}
	return tr, nil
}

// extendSeq 依据上一个扩展序号把 16 位序号提升为 64 位，处理回绕。
func extendSeq(prevExt uint64, havePrev bool, seq16 uint16) uint64 {
	if !havePrev {
		return uint64(seq16)
	}
	rollover := prevExt >> 16
	prev16 := uint16(prevExt)
	switch {
	case seq16 < prev16 && (uint32(prev16)-uint32(seq16)) > (1<<15):
		rollover++ // 正向回绕
	case seq16 > prev16 && (uint32(seq16)-uint32(prev16)) > (1<<15):
		if rollover > 0 {
			rollover-- // 罕见的反向回绕
		}
	}
	return (rollover << 16) | uint64(seq16)
}
