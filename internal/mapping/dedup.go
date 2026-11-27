package mapping

import (
	"relay/internal/media"
)

// 判定结果类别。
const (
	DecisionAccept    = "accept"    // 新序号，正常进入时间线
	DecisionDuplicate = "duplicate" // 同序号且哈希相同，幂等忽略
	DecisionConflict  = "conflict"  // 同序号但哈希不同，隔离且不得覆盖
)

// Decision 是一帧在轨道去重状态机上的判定。
type Decision struct {
	Kind     string
	TrackID  uint32
	CameraID uint16
	Sequence uint64
	Existing [32]byte // 冲突时已登记帧的哈希
	Incoming [32]byte
}

// Dedup 在每条轨道内维护 序号 -> 载荷哈希 的确定性账本。
// 账本全量驻留内存并随检查点持久化，保证中断恢复后判定一致。
type Dedup struct {
	hashes map[uint32]map[uint64][32]byte
}

// NewDepup 创建空账本。
func NewDedup() *Dedup {
	return &Dedup{hashes: make(map[uint32]map[uint64][32]byte)}
}

// Classify 对已解析轨道的帧做判定；accept 时同时登记哈希。
// 该方法对同一 (track,seq) 重复调用是确定性的。
func (d *Dedup) Classify(trackID uint32, f *media.Frame) Decision {
	dec := Decision{TrackID: trackID, CameraID: f.CameraID, Sequence: f.Sequence, Incoming: f.SHA256}
	ledger, ok := d.hashes[trackID]
	if !ok {
		ledger = make(map[uint64][32]byte)
		d.hashes[trackID] = ledger
	}
	prev, exists := ledger[f.Sequence]
	switch {
	case !exists:
		ledger[f.Sequence] = f.SHA256
		dec.Kind = DecisionAccept
	case prev == f.SHA256:
		dec.Kind = DecisionDuplicate
	default:
		dec.Kind = DecisionConflict
		dec.Existing = prev
	}
	return dec
}

// Snapshot 返回可序列化的账本副本（检查点用）。
func (d *Dedup) Snapshot() map[uint32]map[uint64][32]byte {
	out := make(map[uint32]map[uint64][32]byte, len(d.hashes))
	for track, ledger := range d.hashes {
		cp := make(map[uint64][32]byte, len(ledger))
		for seq, h := range ledger {
			cp[seq] = h
		}
		out[track] = cp
	}
	return out
}

// Restore 从检查点账本重建状态（覆盖当前内容）。
func (d *Dedup) Restore(snap map[uint32]map[uint64][32]byte) {
	d.hashes = make(map[uint32]map[uint64][32]byte, len(snap))
	for track, ledger := range snap {
		cp := make(map[uint64][32]byte, len(ledger))
		for seq, h := range ledger {
			cp[seq] = h
		}
		d.hashes[track] = cp
	}
}

// TrackCount 返回已登记轨道数（测试/指标用）。
func (d *Dedup) TrackCount() int { return len(d.hashes) }
