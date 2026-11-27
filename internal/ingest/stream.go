package ingest

import (
	"io"
	"os"

	"medpipe/internal/clock"
	"medpipe/internal/index"
)

// Streamer 按索引有界读取 WAV 数据段，切成定长包并交给重排器。
type Streamer struct {
	idx      *index.Index
	reorder  *Reorder
	packetN  int64
	bpf      int // 每帧字节
	framePos int64
}

// NewStreamer 校验所有块的音频几何一致后构造读取器。
func NewStreamer(idx *index.Index, r *Reorder, packetFrames int64) (*Streamer, error) {
	if len(idx.Blocks) == 0 {
		return nil, clock.NewError(clock.CodeInvalidParam, -1, "ingest: 索引为空")
	}
	base := idx.Blocks[0]
	for _, b := range idx.Blocks[1:] {
		if b.SampleRate != base.SampleRate || b.Channels != base.Channels ||
			b.BitsPerSample != base.BitsPerSample || b.BlockAlign != base.BlockAlign {
			return nil, clock.NewError(clock.CodeInvalidParam, b.ID,
				"ingest: 块 %d 音频几何与首块不一致", b.ID)
		}
	}
	if r.cfg.BytesPerFrame != base.BlockAlign {
		return nil, clock.NewError(clock.CodeInvalidParam, -1,
			"ingest: 重排器帧字节 %d 与块 %d 不符", r.cfg.BytesPerFrame, base.BlockAlign)
	}
	return &Streamer{idx: idx, reorder: r, packetN: packetFrames, bpf: base.BlockAlign}, nil
}

// ReadBlock 读取一个块的有效数据段并按定长帧切包，包坐标按全局帧位置生成。
// 截断块保留完整帧，缺失帧的补偿交给上层按同一几何补静音，绝不平移后续块。
func (s *Streamer) ReadBlock(bi index.BlockIndex) ([]*Packet, int64, error) {
	f, err := os.Open(bi.Path)
	if err != nil {
		return nil, 0, clock.WrapError(clock.CodeInvalidHeader, bi.ID, err, "打开块失败")
	}
	defer f.Close()
	if _, err := f.Seek(bi.DataOffset, io.SeekStart); err != nil {
		return nil, 0, clock.WrapError(clock.CodeInvalidHeader, bi.ID, err, "定位数据段失败")
	}
	out := make([]*Packet, 0, bi.Frames/s.packetN+1)
	remaining := bi.Frames
	for remaining > 0 {
		n := s.packetN
		if n > remaining {
			n = remaining
		}
		buf := make([]byte, n*int64(s.bpf))
		if _, err := io.ReadFull(f, buf); err != nil {
			return nil, 0, clock.WrapError(clock.CodeInvalidHeader, bi.ID, err, "数据段读取不足")
		}
		seq := s.framePos / s.packetN
		out = append(out, &Packet{
			Seq:        seq,
			BlockID:    bi.ID,
			StartFrame: s.framePos,
			Frames:     n,
			Source:     SourcePCM,
			SHA256:     HashPacket(buf),
			PCM:        buf,
		})
		s.framePos += n
		remaining -= n
	}
	// 截断缺口：返回缺失帧数，由流水线补静音占位，保持全局坐标不变。
	missing := bi.MissingFrames
	s.framePos += missing
	return out, missing, nil
}

// FramePos 返回读取器当前全局帧位置（含截断缺口）。
func (s *Streamer) FramePos() int64 { return s.framePos }
