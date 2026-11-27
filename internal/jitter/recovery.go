package jitter

import (
	"context"
	"sync"

	"bcastqc/internal/media"
	rtpx "bcastqc/internal/rtp"
)

// RetransmitCache 缓存近期已发送报文，按字节上限淘汰最旧条目。
// 重传请求命中即返回原始报文副本；未命中表示已超出缓存窗口。
// Store 由生产者调用、Lookup 由消费者并发调用，故全程加锁。
type RetransmitCache struct {
	mu       sync.RWMutex
	maxBytes int64
	bytes    int64
	order    []int64
	store    map[int64]rtpx.Packet
}

// NewRetransmitCache 创建字节有界的发送侧重传缓存。
func NewRetransmitCache(maxBytes int64) *RetransmitCache {
	return &RetransmitCache{maxBytes: maxBytes, store: make(map[int64]rtpx.Packet)}
}

// Store 写入报文并按字节上限淘汰最旧条目。
func (c *RetransmitCache) Store(p rtpx.Packet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	size := int64(len(p.Raw))
	for c.bytes+size > c.maxBytes && len(c.order) > 0 {
		oldest := c.order[0]
		c.bytes -= int64(len(c.store[oldest].Raw))
		delete(c.store, oldest)
		c.order = c.order[1:]
	}
	if size > c.maxBytes {
		return // 单包超上限：不缓存，但不影响主链路
	}
	c.order = append(c.order, p.Seq)
	c.store[p.Seq] = p
	c.bytes += size
}

// Lookup 命中返回报文与 true。
func (c *RetransmitCache) Lookup(seq int64) (rtpx.Packet, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.store[seq]
	return p, ok
}

// Bytes 返回当前缓存字节占用。
func (c *RetransmitCache) Bytes() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bytes
}

// Receiver 驱动单条轨道的接收、去重、重传恢复与缺口放弃。
type Receiver struct {
	buf     *Buffer
	cache   *RetransmitCache
	track   uint32
	seen    map[int64]bool
	lost    map[int64]bool
	sealed  bool // 流结束后置位，此后缺口按窗口判定为永久
	mu      sync.Mutex
	output  []rtpx.Packet // pump 持续累积的已恢复有序报文
	gaps    []Gap
	m       Metrics
	latency []int64
}

// NewReceiver 组装缓存与重传缓存。
func NewReceiver(track uint32, queueBytes, cacheBytes, window int64, done <-chan struct{}) *Receiver {
	return &Receiver{
		buf:   NewBuffer(track, queueBytes, window, done),
		cache: NewRetransmitCache(cacheBytes),
		track: track,
		seen:  make(map[int64]bool),
		lost:  make(map[int64]bool),
	}
}

// Buffer 暴露底层抖动缓存。
func (r *Receiver) Buffer() *Buffer { return r.buf }

// Cache 暴露发送侧重传缓存。
func (r *Receiver) Cache() *RetransmitCache { return r.cache }

// Handle 接收一个链路交付事件对应的报文，去重并入队。
// arriveNS 为虚拟到达时间，用于端到端时延统计。
func (r *Receiver) Handle(ctx context.Context, p rtpx.Packet, arriveNS int64) error {
	dup := r.seen[p.Seq]
	if !dup {
		r.seen[p.Seq] = true
	}
	if err := r.buf.Push(ctx, p, dup); err != nil {
		return err
	}
	if !dup {
		r.latency = append(r.latency, arriveNS-p.SendNS)
	}
	return nil
}

// Pump 执行一轮恢复并把可交付报文存入线程安全的产出队列，
// 返回本轮是否取到报文。供与 Handle 并发的排空协程使用。
func (r *Receiver) Pump(ctx context.Context) (bool, error) {
	ready, unrecovered, err := r.Resolve(ctx)
	if err != nil {
		return false, err
	}
	if len(ready) > 0 || len(unrecovered) > 0 {
		r.mu.Lock()
		r.output = append(r.output, ready...)
		r.gaps = append(r.gaps, unrecovered...)
		r.mu.Unlock()
	}
	return len(ready) > 0, nil
}

// Output 返回 pump 累积的全部已恢复有序报文与永久缺口。
func (r *Receiver) Output() ([]rtpx.Packet, []Gap) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pkts := append([]rtpx.Packet(nil), r.output...)
	gaps := append([]Gap(nil), r.gaps...)
	return pkts, gaps
}

// Seal 标记发送流结束。此后 Resolve 才会把超窗口缺口判为永久，
// 运行期间则只要重传缓存命中就持续补齐，避免突发中途误判。
func (r *Receiver) Seal() { r.sealed = true }

// Resolve 处理当前缺口：窗口内尝试重传，超窗口记为未恢复。
// 视频轨道在出现未恢复缺口时，由调用方依据返回的
// Unrecovered 缺口跳至下一关键帧；音频缺口绝不伪造数据。
func (r *Receiver) Resolve(ctx context.Context) (ready []rtpx.Packet, unrecovered []Gap, err error) {
	ready, gaps := r.buf.PopAll()
	for _, g := range gaps {
		// 重传缓存命中：无论缺口年龄都立即补齐（缓存本身已按字节上限
		// 淘汰最旧条目，能查到即说明仍在可恢复范围内）。
		if p, ok := r.cache.Lookup(g.Seq); ok && !r.seen[g.Seq] {
			r.seen[g.Seq] = true
			if e := r.buf.Push(ctx, p, false); e != nil {
				return nil, nil, e
			}
			r.m.Recovered++
			continue
		}
		// 未封口：流仍在传输，缺失包可能随后续到达，永不误判。
		if !r.sealed {
			continue
		}
		// 封口后缓存未命中：超出重传窗口/已淘汰，判为永久缺口。
		if !r.lost[g.Seq] {
			r.lost[g.Seq] = true
			r.m.Unrecovered++
			unrecovered = append(unrecovered, g)
		}
	}
	if len(ready) > 0 || r.m.Recovered > 0 {
		again, ug, e := r.drainAfterRecovery(ctx)
		if e != nil {
			return nil, nil, e
		}
		ready = append(ready, again...)
		unrecovered = append(unrecovered, ug...)
	}
	return ready, unrecovered, nil
}

// drainAfterRecovery 在重传补齐后再次抽取可连续交付的报文。
func (r *Receiver) drainAfterRecovery(ctx context.Context) ([]rtpx.Packet, []Gap, error) {
	ready, gaps := r.buf.PopAll()
	var unrecovered []Gap
	if !r.sealed {
		return ready, nil, nil
	}
	for _, g := range gaps {
		// 缓存仍可命中：交给下一轮 Resolve 重传，不判永久。
		if _, ok := r.cache.Lookup(g.Seq); ok {
			continue
		}
		if !r.lost[g.Seq] {
			r.lost[g.Seq] = true
			r.m.Unrecovered++
			unrecovered = append(unrecovered, g)
		}
	}
	return ready, unrecovered, nil
}

// Finalize 在发送结束后统计：lost = 未恢复 + 已恢复，并计算时延分位。
func (r *Receiver) Finalize() Metrics {
	bm := r.buf.Snapshot()
	r.m.Received = bm.Received
	r.m.Duplicates = bm.Duplicates
	r.m.PeakBufferBytes = bm.PeakBufferBytes
	r.m.Lost = r.m.Recovered + r.m.Unrecovered
	r.m.LatencyP95NS, r.m.MaxLatencyNS = percentiles(r.latency)
	return r.m
}

// Close 关闭底层缓存。
func (r *Receiver) Close() { r.buf.Close() }

// percentiles 返回 P95 与最大值；空输入返回 0。
func percentiles(samples []int64) (p95, max int64) {
	if len(samples) == 0 {
		return 0, 0
	}
	// 拷贝后排序，避免修改调用方切片。
	s := append([]int64(nil), samples...)
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
	idx := len(s) * 95 / 100
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx], s[len(s)-1]
}

// AssertRecoverable 把可恢复缺口包装为带可恢复标记的统一错误。
func AssertRecoverable(g Gap) error {
	return media.NewError(media.StageRecover, media.KindGap,
		"重传窗口内缺口").WithTrack(g.Track).MarkRecoverable()
}
