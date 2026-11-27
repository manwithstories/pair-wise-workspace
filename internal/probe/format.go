// Package probe 负责本地素材的生成与流式读取。
// 素材为自描述二进制容器 master.bin：头部写回 Plan，其后为带同步字的定界记录，
// 读取侧依靠同步字重扫实现截断后的可恢复续读。
package probe

import (
	"encoding/binary"
	"errors"
	"io"

	"bcast/internal/model"
)

// 容器与帧常量。包长硬上限满足“限制包长”的约束。
const (
	FileMagic  = "BCAST1.0"
	SyncWord   uint32 = 0xBC5ABC5A // 记录同步字，截断重扫用
	FrameMagic        = "BF01"     // 帧体魔数，位于 payload 前 4 字节
	HeaderLen         = 8 + 4 + 8 + 1
	RecHdrLen         = 4 + 1 + 1 + 8 + 8 + 4 + 4 + 4 // sync+track+kind+seq+pts+dur+len+crc
	MaxPacketLen      = 4096
	VideoPayloadLen   = 128
	AudioPayloadLen   = 96
	// 队列与并发上限由各子系统引用，集中声明便于审计。
	QueueCap = 64
)

// ErrFraming 表示容器定界信息损坏且无法在当前位置恢复。
var ErrFraming = errors.New("probe: framing damaged beyond resync")

func u16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func u32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }
func u64(b []byte, v uint64) { binary.BigEndian.PutUint64(b, v) }
func i64(b []byte, v int64)  { binary.BigEndian.PutUint64(b, uint64(v)) }

// WriteHeader 序列化自描述头部：魔数、生成参数、逐轨道时基与回绕模数。
func WriteHeader(w io.Writer, p model.Plan) error {
	buf := make([]byte, 0, 256)
	magic := make([]byte, 8)
	copy(magic, FileMagic)
	buf = append(buf, magic...)
	tmp := make([]byte, 8)
	u32(tmp, uint32(p.Seed))
	buf = append(buf, tmp[:4]...)
	u64(tmp, uint64(p.DurationMS))
	buf = append(buf, tmp...)
	buf = append(buf, uint8(len(p.Tracks)))
	for _, t := range p.Tracks {
		buf = append(buf, t.ID, uint8(t.Kind))
		u64(tmp, t.Timebase)
		buf = append(buf, tmp...)
		i64(tmp, t.PeriodUS)
		buf = append(buf, tmp...)
		i64(tmp, t.JitterUS)
		buf = append(buf, tmp...)
		i64(tmp, t.DriftPPM)
		buf = append(buf, tmp...)
		u64(tmp, p.WrapModulus[t.ID])
		buf = append(buf, tmp...)
	}
	u64(tmp, 0) // 包总数占位，生成结束后 Seek 回填
	buf = append(buf, tmp...)
	_, err := w.Write(buf)
	return err
}

// HeaderSize 返回给定轨道数对应的头部字节数（用于定位回填字段）。
func HeaderSize(trackCount int) int64 {
	// 8 magic + 4 seed + 8 duration + 1 count + tracks*(1+1+8*5) + 8 total
	return int64(HeaderLen + trackCount*42 + 8)
}

// ReadHeader 从 r 解析 Plan 与声明的包总数。
func ReadHeader(r io.Reader) (model.Plan, uint64, error) {
	head := make([]byte, HeaderLen)
	if _, err := io.ReadFull(r, head); err != nil {
		return model.Plan{}, 0, err
	}
	if string(head[:8]) != FileMagic {
		return model.Plan{}, 0, errors.New("probe: bad file magic")
	}
	p := model.Plan{
		Seed:       int64(binary.BigEndian.Uint32(head[8:12])),
		DurationMS: int64(binary.BigEndian.Uint64(head[12:20])),
	}
	n := int(head[20])
	trackBuf := make([]byte, 42)
	p.Tracks = make([]model.TrackPlan, 0, n)
	p.WrapModulus = make(map[uint8]uint64, n)
	for i := 0; i < n; i++ {
		if _, err := io.ReadFull(r, trackBuf); err != nil {
			return model.Plan{}, 0, err
		}
		t := model.TrackPlan{
			ID:       trackBuf[0],
			Kind:     model.TrackKind(trackBuf[1]),
			Timebase: binary.BigEndian.Uint64(trackBuf[2:10]),
			PeriodUS: int64(binary.BigEndian.Uint64(trackBuf[10:18])),
			JitterUS: int64(binary.BigEndian.Uint64(trackBuf[18:26])),
			DriftPPM: int64(binary.BigEndian.Uint64(trackBuf[26:34])),
		}
		p.Tracks = append(p.Tracks, t)
		p.WrapModulus[t.ID] = binary.BigEndian.Uint64(trackBuf[34:42])
	}
	totalBuf := make([]byte, 8)
	if _, err := io.ReadFull(r, totalBuf); err != nil {
		return model.Plan{}, 0, err
	}
	return p, binary.BigEndian.Uint64(totalBuf), nil
}
