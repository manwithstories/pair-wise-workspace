package rtp

import (
	"testing"
	"time"

	pionrtp "github.com/pion/rtp"

	"mediakernel/internal/clock"
)

func pkt(seq uint16, ts uint32) *pionrtp.Packet {
	return &pionrtp.Packet{Header: pionrtp.Header{
		Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: 1,
	}}
}

// drain 在 now 到 end 之间按 1ms 步进驱动缓冲，收集输出。
func drain(b *Buffer, now, end clock.Nanos) []*pionrtp.Packet {
	var out []*pionrtp.Packet
	for ; now <= end; now += clock.Millisecond {
		out = append(out, b.PopDue(now)...)
	}
	return out
}

func TestJitterBufferOrdersAndReorders(t *testing.T) {
	b := NewBuffer(90000, 30*time.Millisecond, 200*time.Millisecond)
	base := clock.Nanos(0)
	// 乱序到达: 1, 0, 2
	b.Push(pkt(1, 90), base+5*clock.Millisecond)
	b.Push(pkt(0, 0), base+10*clock.Millisecond)
	b.Push(pkt(2, 180), base+15*clock.Millisecond)
	out := drain(b, 0, 500*clock.Millisecond)
	if len(out) != 3 || out[0].SequenceNumber != 0 || out[1].SequenceNumber != 1 || out[2].SequenceNumber != 2 {
		t.Fatalf("out of order output: %v", out)
	}
	if b.Stats.Reordered == 0 {
		t.Fatal("expected reorder to be counted")
	}
}

func TestJitterBufferLateDropped(t *testing.T) {
	b := NewBuffer(90000, 20*time.Millisecond, 100*time.Millisecond)
	b.Push(pkt(0, 0), 0)
	// 包 1 在其播放时刻之后才到达 → 迟到丢弃
	late := clock.Nanos(0) + b.maxDelay + 50*clock.Millisecond
	b.Push(pkt(1, 90), late)
	if b.Stats.Late != 1 {
		t.Fatalf("late=%d want 1", b.Stats.Late)
	}
	out := drain(b, 0, 500*clock.Millisecond)
	if len(out) != 1 || out[0].SequenceNumber != 0 {
		t.Fatalf("out=%d", len(out))
	}
}

func TestJitterBufferDuplicateIdempotent(t *testing.T) {
	b := NewBuffer(90000, 20*time.Millisecond, 100*time.Millisecond)
	b.Push(pkt(0, 0), 0)
	b.Push(pkt(0, 0), 0) // 重复派发
	out := drain(b, 0, clock.Nanos(200*time.Millisecond))
	if len(out) != 1 {
		t.Fatalf("duplicate delivered %d times", len(out))
	}
}

func TestJitterBufferGapRecorded(t *testing.T) {
	b := NewBuffer(90000, 20*time.Millisecond, 100*time.Millisecond)
	b.Push(pkt(0, 0), 0)
	b.Push(pkt(2, 180), clock.Millisecond) // 1 缺失
	out := drain(b, 0, 500*clock.Millisecond)
	if len(out) != 2 {
		t.Fatalf("out=%d", len(out))
	}
	if len(b.Gaps) != 1 || b.Gaps[0] != 1 {
		t.Fatalf("gaps=%v", b.Gaps)
	}
}

func TestJitterAdaptsToSpike(t *testing.T) {
	b := NewBuffer(90000, 20*time.Millisecond, 500*time.Millisecond)
	for i := 0; i < 30; i++ {
		b.Push(pkt(uint16(i), uint32(i*90)), clock.Nanos(i)*clock.Millisecond)
	}
	before := b.MaxDelay()
	// 注入 200ms 突发延迟采样
	b.Push(pkt(30, 30*90), 30*clock.Millisecond+200*clock.Millisecond)
	if b.MaxDelay() <= before {
		t.Fatalf("maxDelay did not adapt: %v -> %v", before, b.MaxDelay())
	}
}

func TestRetransmitCacheEvictsFIFO(t *testing.T) {
	c := NewRetransmitCache(2)
	c.Add(pkt(1, 0))
	c.Add(pkt(2, 0))
	c.Add(pkt(1, 0)) // 幂等
	if c.Len() != 2 {
		t.Fatalf("len=%d", c.Len())
	}
	c.Add(pkt(3, 0)) // 淘汰 1
	if _, ok := c.Get(1); ok {
		t.Fatal("seq 1 should be evicted")
	}
	if _, ok := c.Get(3); !ok {
		t.Fatal("seq 3 missing")
	}
}

func TestRecoveryBurst(t *testing.T) {
	r := NewRecovery(4)
	for i := uint16(10); i < 20; i++ {
		r.Cache.Add(pkt(i, 0))
	}
	// 突发丢失 14..17，其中 14、15 已被淘汰
	got := r.Recover([]uint16{14, 15, 16, 17})
	if len(got) != 2 || r.Recovered != 2 {
		t.Fatalf("recovered=%d got=%d", r.Recovered, len(got))
	}
	if r.Lost() != 2 {
		t.Fatalf("lost=%d", r.Lost())
	}
	if len(r.Gaps) != 1 || r.Gaps[0].From != 14 || r.Gaps[0].To != 15 {
		t.Fatalf("gaps=%v", r.Gaps)
	}
	// 空洞记录带原因
	if r.Gaps[0].Reason == "" {
		t.Fatal("gap missing reason")
	}
}
