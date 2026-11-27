package rtp

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"

	"relay/internal/media"
)

// MaxDatagram 限制单条数据报长度，超过即判定为非法长度（防内存放大）。
const MaxDatagram = 1 << 18 // 256 KiB，远大于任何合法 RTP 包。

// Reader 按 [4 字节大端长度][数据报] 的分帧格式顺序读取。
// 解析得到的 Datagram.Payload 指向内部缓冲，在下一次 Read 后失效；
// 需要长期保留的调用方应自行拷贝。
type Reader struct {
	r       *bufio.Reader
	offset  int64
	count   int64
	buf     []byte
	hdr     [4]byte
	started bool
}

// NewReader 包装输入流。
func NewReader(r io.Reader) *Reader {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 64*1024)
	}
	return &Reader{r: br, buf: make([]byte, 0, 4096)}
}

// Offset 返回当前已消费的字节偏移。
func (r *Reader) Offset() int64 { return r.offset }

// Count 返回已尝试读取（含失败）的数据报条数。
func (r *Reader) Count() int64 { return r.count }

// Read 取下一条数据报。流正常结束返回 io.EOF；
// 任何分帧/解析错误都包装为 *media.Fault，携带偏移与数据报次序。
func (r *Reader) Read() (*Datagram, error) {
	// 读取长度前缀。
	if _, err := io.ReadFull(r.r, r.hdr[:]); err != nil {
		// 流的边界处出现部分前缀：没有更多完整数据报，按正常结束处理。
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, io.EOF
		}
		return nil, err
	}
	start := r.offset
	r.offset += 4
	length := int(binary.BigEndian.Uint32(r.hdr[:]))
	if length < rtpHeaderMin || length > MaxDatagram {
		return nil, media.NewFault(media.FaultFraming, media.SeqUnknown,
			start, r.count, fmt.Sprintf("illegal datagram length %d", length))
	}
	r.count++
	if cap(r.buf) < length {
		r.buf = make([]byte, length)
	}
	r.buf = r.buf[:length]
	if _, err := io.ReadFull(r.r, r.buf); err != nil {
		return nil, media.NewFault(media.FaultFraming, media.SeqUnknown,
			start, r.count-1, "declared length exceeds remaining bytes")
	}
	r.offset += int64(length)
	dg, err := parseDatagram(r.buf, start, r.count-1)
	if err != nil {
		return nil, err
	}
	return dg, nil
}

// ParseDatagram 解析一条不含长度前缀的完整数据报（RTP 或 RTCP）。
// 供按字节偏移随机读取的调用方复用同一套头部解析与 Fault 语义。
func ParseDatagram(b []byte) (*Datagram, error) {
	return parseDatagram(b, 0, 0)
}

// parseDatagram 区分 RTP/RTCP 并解析头部，保证返回的 Fault 带精确偏移。
func parseDatagram(b []byte, offset, index int64) (*Datagram, error) {
	d := &Datagram{Offset: offset, Index: index, RawLen: len(b)}
	if len(b) < rtpHeaderMin {
		return nil, media.NewFault(media.FaultTruncated, media.SeqUnknown,
			offset, index, "datagram shorter than RTP minimum header")
	}
	version := b[0] >> 6
	if version != rtpVersion {
		// 版本号非法时仍尽力提取序号，保证坏包能被精确定位（误差≤1包）。
		seq := media.SeqUnknown
		if len(b) >= 4 {
			seq = binary.BigEndian.Uint16(b[2:4])
		}
		return nil, media.NewFault(media.FaultBadPayload, seq,
			offset, index, fmt.Sprintf("bad RTP version %d", version))
	}
	// RTP/RTCP 共用 V=2 首字节，靠第二字节最高位区分：
	// RTCP 报文类型为 200..207（≥128），该位恒为 1；
	// RTP 的 marker 位在动态 PT（96..127）场景下本工程不置位。
	if b[1]&markerBit != 0 {
		return parseRTCP(b, d, offset, index)
	}
	return parseRTP(b, d, offset, index)
}

func parseRTP(b []byte, d *Datagram, offset, index int64) (*Datagram, error) {
	d.Kind, d.IsRTCP = KindRTP, false
	d.Version = b[0] >> 6
	csrcCount := int(b[0] & rtpCSRCOffset)
	hasExt := b[0]&rtpExtBit != 0
	padding := b[0]&rtpPaddingBit != 0
	d.Marker = b[1]&markerBit != 0
	d.PayloadType = b[1] & ptMask
	d.Seq = binary.BigEndian.Uint16(b[2:4])
	d.Timestamp = binary.BigEndian.Uint32(b[4:8])
	d.SSRC = binary.BigEndian.Uint32(b[8:12])
	pos := rtpHeaderMin + 4*csrcCount
	if pos > len(b) {
		return nil, media.NewFault(media.FaultTruncated, d.Seq, offset, index,
			"CSRC list exceeds datagram")
	}
	if hasExt {
		if pos+4 > len(b) {
			return nil, media.NewFault(media.FaultTruncated, d.Seq, offset, index,
				"extension header truncated")
		}
		extWords := int(binary.BigEndian.Uint16(b[pos+2 : pos+4]))
		pos += 4 + 4*extWords
		if pos > len(b) {
			return nil, media.NewFault(media.FaultTruncated, d.Seq, offset, index,
				"extension body exceeds datagram")
		}
	}
	padBytes := 0
	if padding {
		padBytes = int(b[len(b)-1])
		if padBytes <= 0 || padBytes > len(b)-pos {
			return nil, media.NewFault(media.FaultBadPayload, d.Seq, offset, index,
				"illegal padding length")
		}
	}
	end := len(b) - padBytes
	if end <= pos {
		return nil, media.NewFault(media.FaultBadPayload, d.Seq, offset, index,
			"empty RTP payload")
	}
	d.Payload = b[pos:end]
	return d, nil
}

func parseRTCP(b []byte, d *Datagram, offset, index int64) (*Datagram, error) {
	d.Kind, d.IsRTCP = KindRTCP, true
	d.PayloadType = b[1] // RTCP 报文类型（200..207），不做 marker 掩码
	if len(b) < rtcpHeaderMin {
		return nil, media.NewFault(media.FaultTruncated, media.SeqUnknown,
			offset, index, "RTCP header truncated")
	}
	// 长度字段为该报告除去 4 字节头之后的 32 位字数。
	declared := 4 + 4*int(binary.BigEndian.Uint16(b[2:4]))
	if declared != len(b) {
		return nil, media.NewFault(media.FaultTruncated, media.SeqUnknown,
			offset, index, fmt.Sprintf("RTCP length %d != datagram %d", declared, len(b)))
	}
	if d.PayloadType != ptRTCPSender || len(b) < rtcpSRMin {
		// 非 SR 的 RTCP（RR/SDES 等）本工程不使用，保留头部信息即可。
		d.Payload = b[rtcpHeaderMin:]
		return d, nil
	}
	d.SSRC = binary.BigEndian.Uint32(b[4:8])
	d.SRNTPSeconds = binary.BigEndian.Uint32(b[8:12])
	d.SRNTPFraction = binary.BigEndian.Uint32(b[12:16])
	d.SRRTP = binary.BigEndian.Uint32(b[16:20])
	d.SRPacketCount = binary.BigEndian.Uint32(b[20:24])
	d.SROctetCount = binary.BigEndian.Uint32(b[24:28])
	return d, nil
}
