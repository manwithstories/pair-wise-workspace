package jitter

import (
	"context"
	"runtime"
	"testing"
	"time"

	"bcastqc/internal/media"
	rtpx "bcastqc/internal/rtp"
)

func pkt(seq int64, size int) rtpx.Packet {
	return rtpx.Packet{
		Seq: seq, SendNS: seq * 1_000_000, TimeNS: seq * 1_000_000,
		Track: media.TrackVideo, ClockRate: media.VideoClockRate,
		Raw: make([]byte, size),
	}
}

func TestDedupAndInOrderDelivery(t *testing.T) {
	rcv := NewReceiver(media.TrackVideo, 4096, 4096, 8, context.Background().Done())
	ctx := context.Background()
	// 乱序 + 重复输入。
	order := []int64{1, 3, 2, 3, 4, 6, 5}
	for _, s := range order {
		if err := rcv.Handle(ctx, pkt(s, 50), s*1_000_000+500_000); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}
	ready, _, err := rcv.Resolve(ctx)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var seqs []int64
	for _, p := range ready {
		seqs = append(seqs, p.Seq)
	}
	want := []int64{1, 2, 3, 4, 5, 6}
	if len(seqs) != len(want) {
		t.Fatalf("delivered %v, want %v", seqs, want)
	}
	for i := range want {
		if seqs[i] != want[i] {
			t.Fatalf("delivered %v, want %v", seqs, want)
		}
	}
	m := rcv.Finalize()
	if m.Duplicates != 1 {
		t.Fatalf("duplicates=%d want 1", m.Duplicates)
	}
}

func TestRetransmitWithinWindow(t *testing.T) {
	rcv := NewReceiver(media.TrackVideo, 4096, 4096, 10, context.Background().Done())
	ctx := context.Background()
	// 发送侧缓存全部报文，但链路上 seq=3 丢失。
	for s := int64(1); s <= 8; s++ {
		p := pkt(s, 40)
		rcv.Cache().Store(p)
		if s == 3 {
			continue
		}
		if err := rcv.Handle(ctx, p, s*1_000_000); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}
	if _, _, err := rcv.Resolve(ctx); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	m := rcv.Finalize()
	if m.Recovered != 1 || m.Unrecovered != 0 {
		t.Fatalf("recovered=%d unrecovered=%d", m.Recovered, m.Unrecovered)
	}
}

func TestGapExpiresOutsideWindow(t *testing.T) {
	// 窗口仅 2，缺口在 seq=2，最新到 seq=10 时年龄超限。
	rcv := NewReceiver(media.TrackVideo, 4096, 4096, 2, context.Background().Done())
	ctx := context.Background()
	for s := int64(1); s <= 10; s++ {
		if s == 2 {
			continue // 永久缺失，且重传缓存也不存放
		}
		if err := rcv.Handle(ctx, pkt(s, 30), s*1_000_000); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}
	_, unrecovered, err := rcv.Resolve(ctx)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	found := false
	for _, g := range unrecovered {
		if g.Seq == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expired gap not reported: %+v", unrecovered)
	}
	m := rcv.Finalize()
	if m.Unrecovered != 1 || m.Lost != 1 {
		t.Fatalf("metrics wrong: %+v", m)
	}
}

func TestByteBoundBlocksAndBackpressure(t *testing.T) {
	// 上限 200 字节、单包 120 字节：缓存容纳 1 个后，第二个 Push 阻塞。
	doneCh := make(chan struct{})
	rcv := NewReceiver(media.TrackVideo, 200, 200, 8, doneCh)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rcv.Handle(ctx, pkt(1, 120), 1_000_000); err != nil {
		t.Fatal(err)
	}
	pushDone := make(chan error, 1)
	go func() { pushDone <- rcv.Handle(ctx, pkt(2, 120), 2_000_000) }()
	// 等待生产者进入“缓存已满”的阻塞状态。
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		rcv.buf.mu.Lock()
		full := rcv.buf.bytes+120 > rcv.buf.maxBytes
		rcv.buf.mu.Unlock()
		if full {
			break
		}
		runtime.Gosched()
	}
	// 消费者取走缓存中的报文，腾出空间，阻塞的 Push 应解除。
	if _, _, err := rcv.Resolve(ctx); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	select {
	case err := <-pushDone:
		if err != nil {
			t.Fatalf("blocked push should release after drain: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("backpressure did not release after consuming a packet")
	}
	m := rcv.Finalize()
	if m.PeakBufferBytes > 200 {
		t.Fatalf("peak %d exceeds cap 200", m.PeakBufferBytes)
	}
}

func TestRetransmitCacheEvictsByBytes(t *testing.T) {
	c := NewRetransmitCache(300)
	for s := int64(1); s <= 5; s++ {
		c.Store(pkt(s, 100))
	}
	if c.Bytes() > 300 {
		t.Fatalf("cache bytes %d over cap", c.Bytes())
	}
	if _, ok := c.Lookup(1); ok {
		t.Fatal("oldest packet should have been evicted")
	}
	if _, ok := c.Lookup(5); !ok {
		t.Fatal("newest packet should remain")
	}
}

func TestPercentile95(t *testing.T) {
	var samples []int64
	for i := int64(1); i <= 100; i++ {
		samples = append(samples, i*1_000_000)
	}
	p95, max := percentiles(samples)
	// 实现采用零基最近秩 idx=floor(0.95*n)=95，对应第 96 个样本（值 96ms）。
	if p95 != 96_000_000 || max != 100_000_000 {
		t.Fatalf("p95=%d max=%d", p95, max)
	}
}
