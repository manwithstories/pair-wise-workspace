package transport

import (
	"mediamux/media"
)

// Class 是包进入抖动缓冲时的分类。
type Class int

// 包分类。
const (
	ClassOK   Class = iota // 按序到达或窗口内乱序
	ClassDup               // 重复包
	ClassLate              // 迟到包（对应序号已交付或已判失）
)

func (c Class) String() string {
	switch c {
	case ClassDup:
		return "duplicate"
	case ClassLate:
		return "late"
	default:
		return "ok"
	}
}

// Delivery 是抖动缓冲的一次交付。
type Delivery struct {
	Pkt       *Packet // nil 表示缺口
	Gap       bool    // 该交付为丢包缺口标记
	Recovered bool    // 乱序到达但在窗口内恢复
}

// Decision 记录一次窗口自适应决策及其依据的统计量。
type Decision struct {
	At      media.Ticks
	Jitter  media.Ticks
	Window  media.Ticks
	Pending int
	Reason  string
}

// Stats 是抖动缓冲的累计统计。
type Stats struct {
	Received  int
	Duplicate int
	Late      int
	Lost      int
	Recovered int
	PeakBytes int
}

// recentCap 是去重历史的容量（最近交付的序号数）。
const recentCap = 256

// JitterBuffer 实现有界抖动缓冲：重排窗口、超时交付与丢包恢复标记。
// 时间为注入式（media.Ticks），相同输入序列必产生相同输出。
type JitterBuffer struct {
	capacity  int
	clockRate uint32
	minWin    media.Ticks
	maxWin    media.Ticks
	margin    media.Ticks

	window   media.Ticks
	jitter   media.Ticks
	haveTran bool
	lastTran int64

	started  bool
	nextSeq  uint16
	pending  map[uint16]*Packet
	recovSet map[uint16]bool
	gapSince int64 // 首次发现头部缺口的时间；-1 表示无缺口

	recent    []uint16
	recentSet map[uint16]bool
	rhead     int

	forced []Delivery // 容量压力下的强制交付，随下次 Drain/Flush 返回
	bytes  int
	stats  Stats
	log    []Decision
}

// NewJitterBuffer 构造抖动缓冲。capacity 为最大缓存包数（硬上限），
// 窗口在 [minWin, maxWin] 内按抖动估计自适应。
func NewJitterBuffer(capacity int, clockRate uint32, minWin, maxWin media.Ticks) *JitterBuffer {
	return &JitterBuffer{
		capacity:  capacity,
		clockRate: clockRate,
		minWin:    minWin,
		maxWin:    maxWin,
		margin:    minWin / 2,
		window:    minWin,
		gapSince:  -1,
		pending:   make(map[uint16]*Packet),
		recovSet:  make(map[uint16]bool),
		recent:    make([]uint16, 0, recentCap),
		recentSet: make(map[uint16]bool),
	}
}

// Push 将包放入缓冲并返回分类。now 为当前时间（统一时基）。
func (jb *JitterBuffer) Push(pkt *Packet, now media.Ticks) Class {
	jb.stats.Received++
	if !jb.started {
		jb.started = true
		jb.nextSeq = pkt.Seq
	}
	d := SeqDelta(pkt.Seq, jb.nextSeq)
	if d < 0 {
		if jb.recentSet[pkt.Seq] {
			jb.stats.Duplicate++
			return ClassDup
		}
		jb.stats.Late++
		return ClassLate
	}
	if _, ok := jb.pending[pkt.Seq]; ok {
		jb.stats.Duplicate++
		return ClassDup
	}
	jb.observeJitter(pkt, now)
	if d > 0 {
		jb.recovSet[pkt.Seq] = true // 乱序到达，交付时标记恢复
	}
	jb.pending[pkt.Seq] = pkt
	jb.bytes += pkt.Size()
	if jb.bytes > jb.stats.PeakBytes {
		jb.stats.PeakBytes = jb.bytes
	}
	if len(jb.pending) > jb.capacity {
		// 容量压力：强制交付/判失头部，不无限等待也不越界缓存
		for len(jb.pending) > jb.capacity {
			if pkt, ok := jb.pending[jb.nextSeq]; ok {
				jb.forced = append(jb.forced, jb.deliver(pkt))
				continue
			}
			jb.stats.Lost++
			jb.forced = append(jb.forced, Delivery{Gap: true})
			jb.nextSeq++
		}
		jb.gapSince = -1
		jb.log = append(jb.log, Decision{At: now, Jitter: jb.jitter,
			Window: jb.window, Pending: len(jb.pending), Reason: "capacity-overflow"})
	}
	return ClassOK
}

// observeJitter 按 RFC 3550 A.8 估计抖动并自适应窗口，记录决策依据。
func (jb *JitterBuffer) observeJitter(pkt *Packet, now media.Ticks) {
	transit := int64(now) - int64(pkt.MediaTime(jb.clockRate))
	if jb.haveTran {
		d := transit - jb.lastTran
		if d < 0 {
			d = -d
		}
		jb.jitter += (media.Ticks(d) - jb.jitter) / 16
	}
	jb.lastTran = transit
	jb.haveTran = true
	target := 2*jb.jitter + jb.margin
	if target < jb.minWin {
		target = jb.minWin
	}
	if target > jb.maxWin {
		target = jb.maxWin
	}
	if target != jb.window {
		jb.window = target
		jb.log = append(jb.log, Decision{At: now, Jitter: jb.jitter,
			Window: target, Pending: len(jb.pending), Reason: "jitter-update"})
	}
}

// Drain 交付所有到期的包与缺口标记。now 为当前时间。
func (jb *JitterBuffer) Drain(now media.Ticks) []Delivery {
	out := jb.forced
	jb.forced = nil
	for {
		if pkt, ok := jb.pending[jb.nextSeq]; ok {
			out = append(out, jb.deliver(pkt))
			continue
		}
		if len(jb.pending) == 0 {
			break
		}
		if jb.gapSince < 0 {
			jb.gapSince = int64(now)
		}
		if now-media.Ticks(jb.gapSince) < jb.window {
			break // 窗口内继续等待，不无限等待
		}
		out = append(out, Delivery{Gap: true})
		jb.stats.Lost++
		jb.nextSeq++ // 同一突发缺口立即连判，不重复计时
	}
	if len(jb.pending) == 0 {
		jb.gapSince = -1
	} else if _, ok := jb.pending[jb.nextSeq]; ok {
		jb.gapSince = -1
	}
	return out
}

// Flush 在输入结束时交付全部剩余包，缺失序号以缺口标记交付。
func (jb *JitterBuffer) Flush() []Delivery {
	out := jb.forced
	jb.forced = nil
	for len(jb.pending) > 0 {
		if pkt, ok := jb.pending[jb.nextSeq]; ok {
			out = append(out, jb.deliver(pkt))
			continue
		}
		out = append(out, Delivery{Gap: true})
		jb.stats.Lost++
		jb.nextSeq++
	}
	jb.gapSince = -1
	return out
}

func (jb *JitterBuffer) deliver(pkt *Packet) Delivery {
	delete(jb.pending, pkt.Seq)
	jb.bytes -= pkt.Size()
	rec := jb.recovSet[pkt.Seq]
	delete(jb.recovSet, pkt.Seq)
	if rec {
		jb.stats.Recovered++
	}
	jb.noteDelivered(pkt.Seq)
	jb.nextSeq = pkt.Seq + 1
	return Delivery{Pkt: pkt, Recovered: rec}
}

// noteDelivered 记录最近交付序号，用于区分重复与迟到。
func (jb *JitterBuffer) noteDelivered(seq uint16) {
	if len(jb.recent) == recentCap {
		delete(jb.recentSet, jb.recent[jb.rhead])
		jb.recent[jb.rhead] = seq
		jb.rhead = (jb.rhead + 1) % recentCap
	} else {
		jb.recent = append(jb.recent, seq)
	}
	jb.recentSet[seq] = true
}

// Stats 返回累计统计。
func (jb *JitterBuffer) Stats() Stats { return jb.stats }

// Decisions 返回窗口自适应决策日志（含每次决策依据的统计量）。
func (jb *JitterBuffer) Decisions() []Decision { return jb.log }

// Buffered 返回当前缓存包数与字节数（供内存预算核算）。
func (jb *JitterBuffer) Buffered() (packets, bytes int) {
	return len(jb.pending), jb.bytes
}
