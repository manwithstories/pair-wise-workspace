// Package ingest 实现有界流式接入：按索引分块读取 PCM，经去重、重排后
// 向时间线提交有序包；重复包幂等，内容冲突报错，窗口外迟到包拒绝。
package ingest

import (
	"fmt"

	"medpipe/internal/clock"
)

// Source 标记一帧区间的样本来源。
type Source string

const (
	// SourcePCM 来自真实块数据。
	SourcePCM Source = "source"
	// SourceSilence 为缺包/截断补偿的静音。
	SourceSilence Source = "silence"
)

// Packet 是流水线上最小传输单元，坐标采用全局采样帧序号。
type Packet struct {
	Seq        int64  // 全局单调包序号
	BlockID    int64  // 所属块号
	StartFrame int64  // 在统一时间线上的起始采样帧
	Frames     int64  // 包内采样帧数
	Source     Source // source / silence
	SHA256     string // 包内容摘要（静音包为固定零摘要）
	PCM        []byte // 交错排列的帧字节，长度 = Frames*BytesPerFrame
}

// Config 控制重排窗口与内存上限。
type Config struct {
	WindowSize     int64 // 允许超前缓存的最大包数
	MaxBufferBytes int   // 待提交 PCM 的内存上限
	PacketFrames   int64 // 每包标称采样帧数
	TotalFrames    int64 // 时间线总采样帧数（含补偿）
	BytesPerFrame  int   // 声道数 * 每样本字节
}

// Reorder 是有界重排器，零值不可用，须经 New 构造。
type Reorder struct {
	cfg           Config
	pending       map[int64]*Packet
	expect        int64 // 下一个待提交序号
	bufferedBytes int
	duplicates    int64
	late          int64
}

// New 校验配置并构造重排器。
func New(cfg Config) (*Reorder, error) {
	if cfg.WindowSize <= 0 || cfg.PacketFrames <= 0 || cfg.TotalFrames <= 0 || cfg.BytesPerFrame <= 0 {
		return nil, clock.NewError(clock.CodeInvalidParam, -1, "ingest: 非法重排配置 %+v", cfg)
	}
	if cfg.MaxBufferBytes < int(cfg.WindowSize)*cfg.BytesPerFrame*int(cfg.PacketFrames) {
		return nil, clock.NewError(clock.CodeOversize, -1,
			"ingest: 内存上限 %d 无法容纳一个重排窗口", cfg.MaxBufferBytes)
	}
	return &Reorder{cfg: cfg, pending: make(map[int64]*Packet)}, nil
}

// Expect 返回当前待提交序号（即已提交水位）。
func (r *Reorder) Expect() int64 { return r.expect }

// Duplicates 返回被幂等吸收的重复包计数。
func (r *Reorder) Duplicates() int64 { return r.duplicates }

// Late 返回被拒绝的窗口外迟到包计数。
func (r *Reorder) Late() int64 { return r.late }

// Push 接收一个到达包：去重、冲突检测、窗口与内存约束。
func (r *Reorder) Push(p *Packet) error {
	if p == nil || p.Seq < 0 || p.Frames <= 0 {
		return clock.NewError(clock.CodeInvalidParam, p.block(), "ingest: 非法包")
	}
	want := int(p.Frames) * r.cfg.BytesPerFrame
	if p.Source == SourcePCM && len(p.PCM) != want {
		return clock.NewError(clock.CodeInvalidParam, p.BlockID,
			"ingest: 包 %d PCM 长度 %d 与帧几何 %d 不符", p.Seq, len(p.PCM), want)
	}
	// 已提交水位之后的迟到包：任何内容都不得改写已提交样本。
	if p.Seq < r.expect {
		r.late++
		return clock.NewError(clock.CodeLatePacket, p.BlockID,
			"ingest: 包 %d 晚于已提交水位 %d，拒绝改写", p.Seq, r.expect)
	}
	if exist, ok := r.pending[p.Seq]; ok {
		if exist.SHA256 == p.SHA256 {
			r.duplicates++ // 完全相同的重复包：幂等吸收
			return nil
		}
		return clock.NewError(clock.CodeConflict, p.BlockID,
			"ingest: 同序号包 %d 内容冲突", p.Seq)
	}
	if p.Seq-r.expect >= r.cfg.WindowSize {
		return clock.NewError(clock.CodeOversize, p.BlockID,
			"ingest: 包 %d 超出重排窗口 %d（水位 %d）", p.Seq, r.cfg.WindowSize, r.expect)
	}
	if r.bufferedBytes+len(p.PCM) > r.cfg.MaxBufferBytes {
		return clock.NewError(clock.CodeOversize, p.BlockID,
			"ingest: 缓存将达内存上限 %d", r.cfg.MaxBufferBytes)
	}
	r.pending[p.Seq] = p
	r.bufferedBytes += len(p.PCM)
	return nil
}

// Drain 提交所有当前连续可用的包，缺失位置保留空洞（不补静音）。
func (r *Reorder) Drain() []*Packet {
	var out []*Packet
	for {
		p, ok := r.pending[r.expect]
		if !ok {
			return out
		}
		delete(r.pending, r.expect)
		r.bufferedBytes -= len(p.PCM)
		out = append(out, p)
		r.expect++
	}
}

// FlushThrough 声明序号小于 through 的包不会再到达：连续可用者提交，
// 空洞按帧几何补静音并标记来源，水位推进到 through。
func (r *Reorder) FlushThrough(through int64) ([]*Packet, error) {
	if through < r.expect {
		return nil, clock.NewError(clock.CodeInvalidParam, -1,
			"ingest: flush 水位 %d 倒退（当前 %d）", through, r.expect)
	}
	var out []*Packet
	for r.expect < through {
		if p, ok := r.pending[r.expect]; ok {
			delete(r.pending, r.expect)
			r.bufferedBytes -= len(p.PCM)
			out = append(out, p)
			r.expect++
			continue
		}
		s, err := r.silence(r.expect)
		if err != nil {
			return out, err
		}
		out = append(out, s)
		r.expect++
	}
	return out, nil
}

// silence 按序号生成零值补偿包，帧几何与真实包完全一致。
func (r *Reorder) silence(seq int64) (*Packet, error) {
	start := seq * r.cfg.PacketFrames
	if start >= r.cfg.TotalFrames {
		return nil, clock.NewError(clock.CodeInvalidParam, -1,
			"ingest: 静音包序号 %d 越过时间线总长 %d", seq, r.cfg.TotalFrames)
	}
	frames := r.cfg.PacketFrames
	if rest := r.cfg.TotalFrames - start; rest < frames {
		frames = rest
	}
	block := start / r.cfg.PacketFrames // 调用方按块映射时可再修正；默认与序号同构
	return &Packet{
		Seq:        seq,
		BlockID:    block,
		StartFrame: start,
		Frames:     frames,
		Source:     SourceSilence,
		SHA256:     SilenceSHA(frames, r.cfg.BytesPerFrame),
		PCM:        make([]byte, int(frames)*r.cfg.BytesPerFrame),
	}, nil
}

// BufferedBytes 返回当前待提交字节数，供内存监控。
func (r *Reorder) BufferedBytes() int { return r.bufferedBytes }

func (p *Packet) block() int64 {
	if p == nil {
		return -1
	}
	return p.BlockID
}

func (p Packet) String() string {
	return fmt.Sprintf("packet(seq=%d block=%d frames=%d source=%s)", p.Seq, p.BlockID, p.Frames, p.Source)
}
