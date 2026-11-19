// Package rtp 负责采集容器解析、RTP 头解析（pion/rtp）、
// 16bit 序号去重重排、突发缺口估算与带宽自适应等待预算。
package rtp

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"pairflow/internal/clock"
)

// CaptureMagic 是采集文件魔数 PairFlow Capture v1。
var CaptureMagic = [4]byte{'P', 'F', 'C', '1'}

const (
	captureVersion uint32 = 1
	headerSize            = 32
	recordMagic           = byte('R')
	recordTypeRTP         = byte(1)
)

// CaptureHeader 是采集文件 32 字节定长头。
type CaptureHeader struct {
	Magic       [4]byte
	Version     uint32
	CreatedNano int64
	Reserved    [16]byte
}

// CaptureRecord 是一条采集记录：一个 RTP 包及其到达时间、源字节偏移。
type CaptureRecord struct {
	DeviceID     string
	ArrivalNano  int64
	Packet       []byte // 完整 RTP 报文（含头部）
	FileOffset   int64  // 记录在采集文件中的起始字节偏移
	HeaderOffset int64  // 记录头（magic 起）的绝对字节偏移，用作续跑水位
}

// WriteCaptureHeader 写出 32 字节定长采集头。
func WriteCaptureHeader(w io.Writer, createdNano int64) error {
	h := make([]byte, headerSize)
	copy(h[0:4], CaptureMagic[:])
	binary.BigEndian.PutUint32(h[4:8], captureVersion)
	binary.BigEndian.PutUint64(h[8:16], uint64(createdNano))
	_, err := w.Write(h)
	return err
}

// WriteCaptureRecord 写出一条长度前缀的采集记录。
func WriteCaptureRecord(w io.Writer, r CaptureRecord) error {
	if len(r.DeviceID) > 0xffff {
		return errors.New("rtp: device id too long")
	}
	total := 1 + 8 + 2 + len(r.DeviceID) + 4 + len(r.Packet)
	if uint64(total) > 0xffffffff {
		return errors.New("rtp: record too large")
	}
	buf := make([]byte, 0, total+5)
	buf = append(buf, recordMagic)
	buf = binary.BigEndian.AppendUint32(buf, uint32(total))
	buf = append(buf, recordTypeRTP)
	buf = binary.BigEndian.AppendUint64(buf, uint64(r.ArrivalNano))
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(r.DeviceID)))
	buf = append(buf, r.DeviceID...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(r.Packet)))
	buf = append(buf, r.Packet...)
	_, err := w.Write(buf)
	return err
}

// CaptureReader 流式读取采集文件，并保留每条记录的原始字节偏移。
type CaptureReader struct {
	r     *bufio.Reader
	base  int64
	count int64
}

// NewCaptureReader 包装一个输入流；base 为该流在逻辑输入中的起始偏移。
func NewCaptureReader(r io.Reader, base int64) *CaptureReader {
	return &CaptureReader{r: bufio.NewReaderSize(r, 64*1024), base: base}
}

// ReadHeader 读取并校验采集头，返回解析后的头部。
func (c *CaptureReader) ReadHeader() (CaptureHeader, error) {
	var h CaptureHeader
	buf := make([]byte, headerSize)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return h, clock.Wrap("rtp.scan", clock.KindTruncatedHeader, "", c.base,
			clock.NoSeq, "capture header truncated", "重新导出采集文件", err)
	}
	copy(h.Magic[:], buf[0:4])
	h.Version = binary.BigEndian.Uint32(buf[4:8])
	h.CreatedNano = int64(binary.BigEndian.Uint64(buf[8:16]))
	copy(h.Reserved[:], buf[16:32])
	if h.Magic != CaptureMagic {
		return h, clock.New("rtp.scan", clock.KindUnsupportedPayload, "", c.base,
			clock.NoSeq, "bad capture magic", "确认输入为 pairflow 采集文件")
	}
	if h.Version != captureVersion {
		return h, clock.New("rtp.scan", clock.KindUnsupportedPayload, "", c.base,
			clock.NoSeq, fmt.Sprintf("unsupported capture version %d", h.Version),
			"使用匹配版本的采集文件")
	}
	return h, nil
}

// Next 返回下一条记录；io.EOF 表示正常结束。
func (c *CaptureReader) Next() (CaptureRecord, error) {
	var rec CaptureRecord
	off := c.base + c.count
	prefix := make([]byte, 5)
	if _, err := io.ReadFull(c.r, prefix); err != nil {
		if errors.Is(err, io.EOF) {
			return rec, io.EOF
		}
		return rec, clock.Wrap("rtp.scan", clock.KindTruncatedHeader, "", off,
			clock.NoSeq, "record prefix truncated", "忽略尾部截断记录", err)
	}
	c.count += 5
	if prefix[0] != recordMagic {
		return rec, clock.New("rtp.scan", clock.KindUnsupportedPayload, "", off,
			clock.NoSeq, "bad record magic", "跳过损坏文件")
	}
	total := binary.BigEndian.Uint32(prefix[1:5])
	if total < 1+8+2+4 {
		return rec, clock.New("rtp.scan", clock.KindUnsupportedPayload, "", off,
			clock.NoSeq, "record length implausible", "跳过损坏文件")
	}
	body := make([]byte, total)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return rec, clock.Wrap("rtp.scan", clock.KindTruncatedHeader, "", off,
			clock.NoSeq, "record body truncated", "忽略尾部截断记录", err)
	}
	c.count += int64(total)
	pos := 0
	if body[pos] != recordTypeRTP {
		return rec, clock.New("rtp.scan", clock.KindUnsupportedPayload, "", off,
			clock.NoSeq, fmt.Sprintf("unknown record type %d", body[pos]), "跳过该记录")
	}
	pos++
	rec.ArrivalNano = int64(binary.BigEndian.Uint64(body[pos : pos+8]))
	pos += 8
	dl := int(binary.BigEndian.Uint16(body[pos : pos+2]))
	pos += 2
	rec.DeviceID = string(body[pos : pos+dl])
	pos += dl
	pl := int(binary.BigEndian.Uint32(body[pos : pos+4]))
	pos += 4
	if pos+pl != len(body) {
		return rec, clock.New("rtp.scan", clock.KindUnsupportedPayload, "", off,
			clock.NoSeq, "record payload length mismatch", "跳过损坏文件")
	}
	rec.Packet = body[pos : pos+pl]
	rec.HeaderOffset = off
	rec.FileOffset = off + 5 + int64(pos)
	return rec, nil
}

// Count 返回已读取的字节数（不含头部）。
func (c *CaptureReader) Count() int64 { return c.count }
