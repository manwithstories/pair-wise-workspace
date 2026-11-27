package timeline

import (
	"archivemedia/internal/media"
	"archivemedia/internal/packet"
)

// Slot 是时间线上的一个序号槽位，要么承载真实事件，要么是丢失占位。
type Slot struct {
	ExtSeq  int64 // 64 位展开序号
	Event   *packet.Event
	Missing bool
}

// Replayer 维护按展开序号排序的时间线缓冲，负责：
//   - 突发丢失时生成带 FlagMissing 的占位槽位；
//   - 重传包到达后替换占位；
//   - 水位后迟到包触发从最近检查点回溯重算。
//
// 缓冲内容完全由输入事件集合决定，重建顺序不影响最终结果。
type Replayer struct {
	slots     map[int64]*Slot
	clockRate uint32
	// lastContiguous 是当前连续覆盖到的最大展开序号。
	lastContiguous int64
	have           bool
}

// NewReplayer 构造重算器；clockRate 为 RTP 时钟率。
func NewReplayer(clockRate uint32) *Replayer {
	if clockRate == 0 {
		clockRate = 48000
	}
	return &Replayer{slots: make(map[int64]*Slot), clockRate: clockRate}
}

// Insert 放入一个已去重事件（展开序号由调用方用 packet.ExtendSeq 计算）。
// 若该槽位是占位，则替换并返回 replaced=true。
func (r *Replayer) Insert(extSeq int64, e *packet.Event) (replaced bool, err error) {
	if err := e.Validate(); err != nil {
		return false, err
	}
	old, ok := r.slots[extSeq]
	if ok && !old.Missing {
		// 去重器已保证同键唯一；走到这里属于编程错误。
		return false, media.E(media.KindTimeline, "timeline.insert",
			e.Location(), "真实槽位被重复写入", nil)
	}
	r.slots[extSeq] = &Slot{ExtSeq: extSeq, Event: e, Missing: false}
	if ok {
		replaced = true
	}
	r.recomputeContiguous()
	return replaced, nil
}

// FillMissing 在 [from,to] 闭区间生成占位槽位。
// durationTicks 为每个占位的时长；已存在的真实槽位不被覆盖。
// 返回新生成的占位数。
func (r *Replayer) FillMissing(from, to int64, template packet.Event) (int, error) {
	if to < from {
		return 0, media.E(media.KindTimeline, "timeline.fill-missing",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"丢失区间起止倒置", nil)
	}
	created := 0
	for seq := from; seq <= to; seq++ {
		if _, ok := r.slots[seq]; ok {
			continue
		}
		ph := template
		ph.Flags = packet.FlagMissing
		ph.Payload = nil
		ph.Seq = uint16(seq & 0xFFFF)
		if ph.DurationTicks == 0 {
			ph.DurationTicks = r.clockRate / 50 // 默认 20ms 占位
		}
		if err := ph.Validate(); err != nil {
			return created, err
		}
		r.slots[seq] = &Slot{ExtSeq: seq, Event: &ph, Missing: true}
		created++
	}
	r.recomputeContiguous()
	return created, nil
}

// recomputeContiguous 从最小槽位起重新计算连续覆盖边界。
func (r *Replayer) recomputeContiguous() {
	if len(r.slots) == 0 {
		r.have = false
		return
	}
	// 找最小序号。
	minSeq := int64(0)
	for seq := range r.slots {
		if !r.have || seq < minSeq {
			minSeq = seq
		}
	}
	r.have = true
	cursor := minSeq
	for {
		if _, ok := r.slots[cursor]; !ok {
			break
		}
		cursor++
	}
	r.lastContiguous = cursor - 1
}

// ContiguousEdge 返回连续覆盖边界（含）。
func (r *Replayer) ContiguousEdge() (int64, bool) {
	return r.lastContiguous, r.have
}

// MissingRanges 返回当前所有占位区间（展开序号闭区间），确定性排序。
func (r *Replayer) MissingRanges() [][2]int64 {
	var missing []int64
	for seq, s := range r.slots {
		if s.Missing {
			missing = append(missing, seq)
		}
	}
	// 插入排序，避免额外依赖。
	for i := 1; i < len(missing); i++ {
		for j := i; j > 0 && missing[j] < missing[j-1]; j-- {
			missing[j], missing[j-1] = missing[j-1], missing[j]
		}
	}
	var ranges [][2]int64
	for _, seq := range missing {
		if n := len(ranges); n > 0 && ranges[n-1][1]+1 == seq {
			ranges[n-1][1] = seq
		} else {
			ranges = append(ranges, [2]int64{seq, seq})
		}
	}
	return ranges
}

// SortedSlots 返回按展开序号排序的全部槽位快照。
func (r *Replayer) SortedSlots() []*Slot {
	seqs := make([]int64, 0, len(r.slots))
	for seq := range r.slots {
		seqs = append(seqs, seq)
	}
	for i := 1; i < len(seqs); i++ {
		for j := i; j > 0 && seqs[j] < seqs[j-1]; j-- {
			seqs[j], seqs[j-1] = seqs[j-1], seqs[j]
		}
	}
	out := make([]*Slot, 0, len(seqs))
	for _, seq := range seqs {
		out = append(out, r.slots[seq])
	}
	return out
}

// Rewind 模拟从检查点回溯：丢弃所有展开序号 > checkpointSeq 的槽位，
// 并把连续边界回退。返回被丢弃的槽位数。
// 重放时这些槽位会由调用方重新喂入，保证幂等。
func (r *Replayer) Rewind(checkpointSeq int64) int {
	dropped := 0
	for seq, s := range r.slots {
		if seq > checkpointSeq {
			_ = s
			delete(r.slots, seq)
			dropped++
		}
	}
	r.recomputeContiguous()
	return dropped
}

// Len 返回槽位总数（含占位）。
func (r *Replayer) Len() int { return len(r.slots) }

// MissingCount 返回占位槽位数。
func (r *Replayer) MissingCount() int {
	n := 0
	for _, s := range r.slots {
		if s.Missing {
			n++
		}
	}
	return n
}
