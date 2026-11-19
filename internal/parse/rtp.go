package parse

import (
	"context"
	"encoding/binary"
	"io"

	"github.com/pion/rtp"

	"mediarepair/internal/fault"
)

// RTPSample 是一个解析后的 RTP 包的时间戳映射信息。
type RTPSample struct {
	Seq       uint16
	Timestamp uint32 // 32 位 RTP 时间戳（可能回绕）
	SSRC      uint32
	Offset    int64 // dump 文件中的字节偏移
	Size      int
}

// ParseRTPDump 流式解析 RTP 包 dump：每条记录为 [2 字节大端长度][RTP 包]。
// 仅解析包头与时间戳，不发送任何网络数据。
func ParseRTPDump(ctx context.Context, r io.Reader, emit func(RTPSample) error) error {
	var off int64
	var hdr [2]byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		recOff := off
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			if err == io.ErrUnexpectedEOF {
				return fault.New(fault.StageParse, 0, recOff, false, "RTP 记录长度前缀截断")
			}
			return fault.Wrap(fault.StageParse, 0, recOff, false, "读取 RTP 记录失败", err)
		}
		off += 2
		n := binary.BigEndian.Uint16(hdr[:])
		if n < 12 {
			return fault.New(fault.StageParse, 0, recOff, false, "RTP 记录长度非法（小于最小包头）")
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return fault.Wrap(fault.StageParse, 0, recOff, false, "RTP 记录体截断", err)
		}
		off += int64(n)
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf); err != nil {
			return fault.Wrap(fault.StageParse, 0, recOff, true, "RTP 包头解析失败", err)
		}
		if err := emit(RTPSample{
			Seq: pkt.SequenceNumber, Timestamp: pkt.Timestamp,
			SSRC: pkt.SSRC, Offset: recOff, Size: int(n),
		}); err != nil {
			return err
		}
	}
}
