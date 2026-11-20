// Package transport 解析 RTP 包流并实现有界抖动缓冲。
package transport

import (
	"bufio"
	"encoding/binary"
	"io"

	"github.com/pion/rtp"

	"mediamux/media"
)

// Packet 是解析后的 RTP 包视图，带输入偏移与到达时间。
type Packet struct {
	Seq       uint16
	Timestamp uint32
	SSRC      uint32
	Marker    bool
	Payload   []byte
	Offset    int64       // 在输入流中的字节偏移（含 2 字节长度前缀）
	Arrival   media.Ticks // 到达时间（统一时基），由注入方设置
}

// Size 返回包的近似字节数（12 字节头 + 负载），用于内存预算核算。
func (p *Packet) Size() int { return 12 + len(p.Payload) }

// MediaTime 把 RTP 时间戳按时钟频率换算为统一时基的媒体时间（相对原点）。
func (p *Packet) MediaTime(clockRate uint32) media.Ticks {
	return media.Convert(media.Ticks(p.Timestamp), media.Timebase{Num: 1, Den: int64(clockRate)}, media.MediaTimebase)
}

// SeqDelta 返回 a-b 的模 2^16 有符号差（回绕安全）。
func SeqDelta(a, b uint16) int { return int(int16(a - b)) }

// Reader 从「2 字节大端长度 + RTP 包」字节流增量解析 RTP 包，
// 不将完整输入载入内存，每个包都保留原始偏移。
type Reader struct {
	r   *bufio.Reader
	off int64
	max int
}

// NewReader 构造 RTP 流解析器；maxPacket 为单包字节上限。
func NewReader(r io.Reader, maxPacket int) *Reader {
	return &Reader{r: bufio.NewReader(r), max: maxPacket}
}

// Next 读取下一个包；io.EOF 表示结束。长度越界或 RTP 解析失败为
// 可恢复错误（跳过该包继续）；长度前缀或包体截断为不可恢复错误。
func (rd *Reader) Next() (*Packet, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(rd.r, hdr[:]); err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		if err == io.ErrUnexpectedEOF {
			return nil, media.New(media.StageTransport, "rtp", rd.off, false, "长度前缀截断")
		}
		return nil, err
	}
	off := rd.off
	rd.off += 2
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n < 12 || n > rd.max {
		if _, err := io.CopyN(io.Discard, rd.r, int64(n)); err != nil {
			return nil, media.New(media.StageTransport, "rtp", off, false,
				"包长度 %d 越界且流截断", n)
		}
		rd.off += int64(n)
		return nil, media.New(media.StageTransport, "rtp", off, true,
			"包长度 %d 越界（上限 %d）", n, rd.max)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(rd.r, buf); err != nil {
		return nil, media.New(media.StageTransport, "rtp", off, false, "包体截断")
	}
	rd.off += int64(n)
	var p rtp.Packet
	if err := p.Unmarshal(buf); err != nil {
		return nil, media.New(media.StageTransport, "rtp", off, true,
			"RTP 解析失败: %v", err)
	}
	return &Packet{
		Seq:       p.SequenceNumber,
		Timestamp: p.Timestamp,
		SSRC:      p.SSRC,
		Marker:    p.Marker,
		Payload:   p.Payload,
		Offset:    off,
	}, nil
}
