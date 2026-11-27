package rtp

import (
	"sync"

	"relay/internal/media"
)

// Stats 汇总重排序阶段的诊断计数。
type Stats struct {
	InOrder    int64
	Reordered  int64
	Duplicates int64
	Late       int64
	Missing    int64
	Buffered   int
}

// Reorder 在固定序号窗口内对 RTP 数据报重排。
// 重复序号只计数、不重复输出；窗口滑过仍缺失的序号记丢包；
// 窗口外（过早或过晚）的包进入 Late 诊断。线程安全。
type Reorder struct {
	mu      sync.Mutex
	window  int
	next    uint16 // 下一个期望输出的序号
	have    map[uint16]*Datagram
	started bool
	stats   Stats
}

// NewReorder 创建窗口容量为 window（至少 1）的重排器。
func NewReorder(window int) *Reorder {
	if window < 1 {
		window = 1
	}
	return &Reorder{window: window, have: make(map[uint16]*Datagram)}
}

// Stats 返回当前计数快照。
func (r *Reorder) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats
	s.Buffered = len(r.have)
	return s
}

// Push 接收一个数据报，返回本次可按序释放的零到多个数据报。
// 诊断事件通过 onFault 回调上抛（丢包、乱序、重复、迟到），可为 nil。
func (r *Reorder) Push(d *Datagram, onFault func(*media.Fault)) []*Datagram {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.started {
		r.started = true
		r.next = d.Seq
	}
	dist := media.SeqDistance(d.Seq, r.next)

	// 迟到：序号已滑过（next 之前），无法归位。
	if dist < 0 {
		r.stats.Late++
		r.emit(onFault, media.NewFault(media.FaultLate, d.Seq, d.Offset, d.Index,
			"packet arrived after window advanced"))
		return nil
	}
	// 重复：该序号仍在窗口缓存中。
	if _, ok := r.have[d.Seq]; ok {
		r.stats.Duplicates++
		r.emit(onFault, media.NewFault(media.FaultDuplicate, d.Seq, d.Offset, d.Index,
			"duplicate sequence number"))
		return nil
	}
	// 过早：超出窗口容量，缓存会造成无界内存，按越界诊断丢弃。
	if dist > r.window {
		r.stats.Late++
		r.emit(onFault, media.NewFault(media.FaultLate, d.Seq, d.Offset, d.Index,
			"packet beyond reorder window"))
		return nil
	}
	if dist > 0 {
		r.stats.Reordered++
		r.emit(onFault, media.NewFault(media.FaultReordered, d.Seq, d.Offset, d.Index,
			"buffered for reordering"))
	}
	r.have[d.Seq] = d
	return r.drainLocked(onFault)
}

func (r *Reorder) emit(onFault func(*media.Fault), f *media.Fault) {
	if onFault != nil {
		onFault(f)
	}
}

// drainLocked 从 next 起连续释放缓存；缓冲填满窗口仍有缺口时宣布丢包并滑过。
func (r *Reorder) drainLocked(onFault func(*media.Fault)) []*Datagram {
	out := make([]*Datagram, 0, len(r.have))
	for {
		d, ok := r.have[r.next]
		if !ok {
			if len(r.have) >= r.window {
				r.stats.Missing++
				r.emit(onFault, media.NewFault(media.FaultMissing, r.next, -1, -1,
					"gap never filled within window"))
				r.next = media.Wrap16(r.next, 1)
				continue
			}
			break
		}
		delete(r.have, r.next)
		out = append(out, d)
		r.next = media.Wrap16(r.next, 1)
		r.stats.InOrder++
	}
	return out
}

// Flush 在输入结束时强制排空：剩余缺口全部记丢包，缓存包按序输出。
func (r *Reorder) Flush(onFault func(*media.Fault)) []*Datagram {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return nil
	}
	out := make([]*Datagram, 0, len(r.have))
	for len(r.have) > 0 {
		if _, ok := r.have[r.next]; !ok {
			r.stats.Missing++
			r.emit(onFault, media.NewFault(media.FaultMissing, r.next, -1, -1,
				"gap at end of stream"))
			r.next = media.Wrap16(r.next, 1)
			continue
		}
		out = append(out, r.drainLocked(onFault)...)
	}
	return out
}
