package ingest

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"bcastqc/internal/model"
)

// Packet 是一路轨道上的传输包记录。
type Packet struct {
	TrackID int
	// Seq 为轨道内单调递增的包序号。
	Seq int64
	// StartSample 为本包首个采样帧在轨道时间线上的位置。
	StartSample int64
	// Frames 为本包包含的采样帧数。
	Frames int64
	// Anchor 为 true 时该包携带同步锚点（PCM 的关键帧标记）。
	Anchor bool
	// Payload 为 PCM 载荷字节，CRC 基于其内容计算。
	Payload []byte
	// CRC 为发送方记录的 CRC-32（IEEE）；0 表示未附带校验。
	CRC uint32
}

// EndSample 返回半开区间末端采样点。
func (p Packet) EndSample() int64 { return p.StartSample + p.Frames }

// ContentHash 计算包内容指纹，用于相同序号的幂等/冲突判别。
// 指纹覆盖序号以外的全部语义字段与载荷。
func (p Packet) ContentHash() [32]byte {
	h := sha256.New()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(p.StartSample))
	h.Write(buf[:])
	binary.LittleEndian.PutUint64(buf[:], uint64(p.Frames))
	h.Write(buf[:])
	binary.LittleEndian.PutUint32(buf[:4], p.CRC)
	h.Write(buf[:4])
	if p.Anchor {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	h.Write(p.Payload)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// PacketRecord 是去重整理后的规范化记录。
type PacketRecord struct {
	Packet
	Hash [32]byte
}

// ReorderStats 汇总包序整理的统计信息。
type ReorderStats struct {
	Received   int
	Duplicates int
	Conflicts  int
	// LostSeqs 为确认丢失（超窗未到达）的序号列表，按轨道分组。
	LostSeqs map[int][]int64
	// Reordered 为到达时偏离期望序号的包数量。
	Reordered int
}

// WindowAssembler 在固定乱序窗口内整理单条轨道的包。
// 内存占用只随窗口大小增长，与素材总长度无关。
type WindowAssembler struct {
	trackID  int
	window   int64
	nextSeq  int64
	expected int64 // 下一个期望释放的序号
	pending  map[int64]PacketRecord
	released []PacketRecord
	seen     map[int64][32]byte
	stats    ReorderStats
}

// NewWindowAssembler 创建组装器；window 为允许的乱序跨度（包数）。
func NewWindowAssembler(trackID int, window int64) *WindowAssembler {
	if window < 1 {
		window = 1
	}
	return &WindowAssembler{
		trackID: trackID,
		window:  window,
		pending: make(map[int64]PacketRecord),
		seen:    make(map[int64][32]byte),
		stats:   ReorderStats{LostSeqs: make(map[int][]int64)},
	}
}

// AddPacket 接收一个到达的包，执行去重、冲突检测与窗口约束。
func (a *WindowAssembler) AddPacket(p Packet) error {
	if p.TrackID != a.trackID {
		return fmt.Errorf("%w: 包轨道 %d 与组装器 %d 不一致",
			model.ErrInvalidInput, p.TrackID, a.trackID)
	}
	if p.Frames <= 0 || p.StartSample < 0 {
		return fmt.Errorf("%w: 非法包范围 start=%d frames=%d",
			model.ErrInvalidInput, p.StartSample, p.Frames)
	}
	if err := model.CheckSampleCount(p.EndSample()); err != nil {
		return err
	}
	rec := PacketRecord{Packet: p, Hash: p.ContentHash()}

	if prior, ok := a.seen[p.Seq]; ok {
		if prior == rec.Hash {
			a.stats.Duplicates++ // 完全相同的包：幂等丢弃。
			return nil
		}
		a.stats.Conflicts++
		return fmt.Errorf("%w: 轨道 %d 序号 %d 出现异内容包",
			model.ErrPacketConflict, p.TrackID, p.Seq)
	}
	a.seen[p.Seq] = rec.Hash
	if p.Seq < a.expected {
		// 迟到且已越过释放点的序号无法再纳入有序流。
		a.stats.Reordered++
		return nil
	}
	if _, ok := a.pending[p.Seq]; ok {
		a.stats.Duplicates++
		return nil
	}
	if p.Seq != a.expected {
		a.stats.Reordered++
	}
	a.pending[p.Seq] = rec
	a.stats.Received++
	a.advance()
	return nil
}

// advance 释放连续可用的包；当缺口超出乱序窗口时判定丢失并跳过。
func (a *WindowAssembler) advance() {
	for {
		if rec, ok := a.pending[a.expected]; ok {
			a.released = append(a.released, rec)
			delete(a.pending, a.expected)
			a.expected++
			continue
		}
		// 计算窗口内最高序号，判断缺口是否已超窗。
		var maxSeq int64 = a.expected - 1
		for seq := range a.pending {
			if seq > maxSeq {
				maxSeq = seq
			}
		}
		if maxSeq-a.expected >= a.window {
			// expected 确认丢失：记录并跳过，后续采样位置保持不挤移。
			a.stats.LostSeqs[a.trackID] = append(a.stats.LostSeqs[a.trackID], a.expected)
			a.expected++
			continue
		}
		return
	}
}

// Flush 在所有包到达后收尾：仍未补齐的序号全部判丢失并释放缓存。
func (a *WindowAssembler) Flush() ([]PacketRecord, ReorderStats) {
	for len(a.pending) > 0 {
		if rec, ok := a.pending[a.expected]; ok {
			a.released = append(a.released, rec)
			delete(a.pending, a.expected)
			a.expected++
			continue
		}
		a.stats.LostSeqs[a.trackID] = append(a.stats.LostSeqs[a.trackID], a.expected)
		a.expected++
	}
	sort.Slice(a.released, func(i, j int) bool { return a.released[i].Seq < a.released[j].Seq })
	return a.released, a.stats
}

// Stats 返回当前统计快照。
func (a *WindowAssembler) Stats() ReorderStats { return a.stats }

// PendingCount 返回窗口内缓存的包数（内存上界指标）。
func (a *WindowAssembler) PendingCount() int { return len(a.pending) }
