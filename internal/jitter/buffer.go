// Package jitter 实现字节上限受控的抖动缓存：按序去重、
// 重传窗口内恢复缺口、过期缺口上报，并在下游阻塞时背压。
package jitter

import (
	"context"
	"sort"
	"sync"

	"bcastqc/internal/media"
	rtpx "bcastqc/internal/rtp"
)

// Metrics 汇总单条轨道在恢复阶段的可观测指标。
type Metrics struct {
	Received         int   // 实际进入缓存的报文数（去重后）
	Duplicates       int   // 被丢弃的重复报文数
	Lost             int   // 检测到的缺失序列号总数
	Recovered        int   // 经重传补齐的数量
	Unrecovered      int   // 窗口外永久缺口数量
	PeakBufferBytes  int64 // 排队报文的峰值字节占用
	LatencyP95NS     int64 // 端到端时延 95 分位
	MaxLatencyNS     int64
}

// Buffer 是单条轨道的字节有界抖动缓存。
type Buffer struct {
	track     uint32
	maxBytes  int64
	window    int64 // 重传窗口（序列号跨度）
	mu        sync.Mutex
	cond      *sync.Cond
	items     map[int64]rtpx.Packet
	bytes     int64
	peak      int64
	nextOut   int64 // 下一个期望交付的展开序列号
	started   bool
	closed    bool
	ctxDone   <-chan struct{}
	latencies []int64
	m         Metrics
}

// NewBuffer 创建抖动缓存。window 为允许重传的序列号跨度。
func NewBuffer(track uint32, maxBytes int64, window int64, done <-chan struct{}) *Buffer {
	b := &Buffer{
		track:    track,
		maxBytes: maxBytes,
		window:   window,
		items:    make(map[int64]rtpx.Packet),
		ctxDone:  done,
	}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// Push 放入一个到达报文。超出字节上限时阻塞等待空间（背压），
// 直到上下文取消返回 KindCanceled。
func (b *Buffer) Push(ctx context.Context, p rtpx.Packet, dup bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if dup {
		b.m.Duplicates++
		return nil
	}
	if b.started && p.Seq < b.nextOut {
		// 已交付过的迟到重复。
		b.m.Duplicates++
		return nil
	}
	size := int64(len(p.Raw))
	for b.bytes+size > b.maxBytes && !b.closed {
		if ctx.Err() != nil || b.canceled() {
			return media.NewError(media.StageRecover, media.KindCanceled,
				"等待缓存空间时被取消")
		}
		b.cond.Wait()
	}
	if b.closed {
		return media.NewError(media.StageRecover, media.KindCanceled, "缓存已关闭")
	}
	if _, exists := b.items[p.Seq]; exists {
		b.m.Duplicates++
		return nil
	}
	b.items[p.Seq] = p
	b.bytes += size
	b.m.Received++
	if b.bytes > b.peak {
		b.peak = b.bytes
	}
	if !b.started {
		b.started = true
		b.nextOut = p.Seq
	}
	b.cond.Broadcast()
	return nil
}

func (b *Buffer) canceled() bool {
	select {
	case <-b.ctxDone:
		return true
	default:
		return false
	}
}

// PopAll 取出当前可按序交付的连续报文（含重传补齐部分），
// 同时返回检测到的缺口区间，供恢复逻辑决定重传或放弃。
func (b *Buffer) PopAll() (ready []rtpx.Packet, gaps []Gap) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.started || len(b.items) == 0 {
		return nil, nil
	}
	seqs := make([]int64, 0, len(b.items))
	for s := range b.items {
		seqs = append(seqs, s)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	maxSeq := seqs[len(seqs)-1]
	have := make(map[int64]bool, len(seqs))
	for _, s := range seqs {
		have[s] = true
	}
	// 统计 nextOut..maxSeq 之间的缺口。
	for s := b.nextOut; s <= maxSeq; s++ {
		if !have[s] {
			gaps = append(gaps, Gap{Track: b.track, Seq: s,
				Age: maxSeq - s})
		}
	}
	// 交付从 nextOut 起的连续前缀。
	for have[b.nextOut] {
		p := b.items[b.nextOut]
		ready = append(ready, p)
		delete(b.items, b.nextOut)
		b.bytes -= int64(len(p.Raw))
		b.recordLatency(p)
		b.nextOut++
	}
	b.cond.Broadcast()
	return ready, gaps
}

func (b *Buffer) recordLatency(p rtpx.Packet) {
	// 时延以虚拟时钟差衡量；到达时间由上层在 Packet 外记账，
	// 这里先用 SendNS 占位，实际值在 Receiver 中回填。
	if p.SendNS > 0 {
		b.latencies = append(b.latencies, p.SendNS)
	}
}

// Has 判断某序列号是否已在缓存或已交付。
func (b *Buffer) Has(seq int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.items[seq]; ok {
		return true
	}
	return b.started && seq < b.nextOut
}

// NextOut 暴露当前期望序列号，供重传判定。
func (b *Buffer) NextOut() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nextOut
}

// Window 返回重传窗口跨度。
func (b *Buffer) Window() int64 { return b.window }

// Close 解除所有阻塞的 Push。
func (b *Buffer) Close() {
	b.mu.Lock()
	b.closed = true
	b.cond.Broadcast()
	b.mu.Unlock()
}

// Snapshot 返回当前指标快照。
func (b *Buffer) Snapshot() Metrics {
	b.mu.Lock()
	defer b.mu.Unlock()
	mm := b.m
	mm.PeakBufferBytes = b.peak
	return mm
}
