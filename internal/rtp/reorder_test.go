package rtp

import (
	"errors"
	"testing"

	"github.com/pion/rtp"
)

func pkt(seq uint16, ts uint32, payload byte) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: seq, Timestamp: ts},
		Payload: []byte{payload},
	}
}

func TestWindowBounds(t *testing.T) {
	if _, err := NewReorderBuffer(0); err == nil {
		t.Fatal("zero window must be rejected")
	}
	if _, err := NewReorderBuffer(MaxReorderWindow + 1); err == nil {
		t.Fatal("oversize window must be rejected")
	}
	if _, err := NewReorderBuffer(64); err != nil {
		t.Fatalf("valid window rejected: %v", err)
	}
}

func TestInOrderDelivery(t *testing.T) {
	rb, _ := NewReorderBuffer(8)
	for i := uint16(0); i < 5; i++ {
		if err := rb.Push(pkt(i, uint32(i)*160, byte(i))); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	out := rb.PopAll()
	if len(out) != 5 {
		t.Fatalf("delivered %d packets", len(out))
	}
	for i, p := range out {
		if p.SequenceNumber != uint16(i) {
			t.Fatalf("order broken at %d: seq %d", i, p.SequenceNumber)
		}
	}
}

func TestReorderAndGap(t *testing.T) {
	rb, _ := NewReorderBuffer(8)
	// 到达顺序：0,1,3,2 -> 交付应按 0,1,2,3，且 seq 2 到达前存在缺口。
	_ = rb.Push(pkt(0, 0, 0))
	_ = rb.Push(pkt(1, 160, 1))
	_ = rb.Push(pkt(3, 480, 3))
	if got := rb.PopAll(); len(got) != 2 {
		t.Fatalf("expected 2 in-order before gap fill, got %d", len(got))
	}
	if len(rb.Gaps()) != 0 {
		t.Fatal("gap should only register when base advances across it")
	}
	_ = rb.Push(pkt(2, 320, 2))
	out := rb.PopAll()
	if len(out) != 2 || out[0].SequenceNumber != 2 || out[1].SequenceNumber != 3 {
		t.Fatalf("gap fill order wrong: %v", out)
	}
}

func TestBurstLossProducesContiguousGap(t *testing.T) {
	rb, _ := NewReorderBuffer(16)
	_ = rb.Push(pkt(10, 1000, 10))
	// 缺失 11..14（突发丢失 4 包），远侧包 15 仍在同一窗口内，
	// 缺口在 Flush 时依据已知序号空间确认。
	_ = rb.Push(pkt(15, 1800, 15))
	out := rb.PopAll() // 10 交付后基准停在缺失的 11
	if len(out) != 1 || out[0].SequenceNumber != 10 {
		t.Fatalf("delivery before gap: %v", out)
	}
	tail := rb.Flush() // 11..14 合并为带右邻的缺口，并交付 15
	if len(tail) != 1 || tail[0].SequenceNumber != 15 {
		t.Fatalf("delivery after burst: %v", tail)
	}
	gaps := rb.Gaps()
	if len(gaps) != 1 {
		t.Fatalf("gaps=%d, want 1", len(gaps))
	}
	g := gaps[0]
	if g.SeqStart != 11 || g.SeqEnd != 14 ||
		!g.HasNext || g.PrevTS != 1000 || g.NextTS != 1800 {
		t.Fatalf("gap detail wrong: %+v", g)
	}
}

func TestDuplicateAndConflict(t *testing.T) {
	rb, _ := NewReorderBuffer(8)
	first := pkt(5, 800, 1)
	if err := rb.Push(first); err != nil {
		t.Fatal(err)
	}
	// 完全一致的重复。
	if err := rb.Push(pkt(5, 800, 1)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("expected duplicate, got %v", err)
	}
	// 同序号不同载荷 -> 冲突损坏。
	if err := rb.Push(pkt(5, 800, 9)); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	dup, conflict, _ := rb.Stats()
	if dup != 1 || conflict != 1 {
		t.Fatalf("stats dup=%d conflict=%d", dup, conflict)
	}
}

func TestLateOutsideWindow(t *testing.T) {
	rb, _ := NewReorderBuffer(4)
	_ = rb.Push(pkt(0, 0, 0))
	// 与基准距离 >= 窗口宽度：远侧包保留，但滑窗把 0 之前空间定性，
	// 随后真正的旧序号迟到被拒绝。
	_ = rb.Push(pkt(5, 800, 5))
	rb.PopAll()
	// 0 已交付后再次到达：窗口外迟到。
	if err := rb.Push(pkt(0, 0, 0)); !errors.Is(err, ErrLateOutside) {
		t.Fatalf("old seq late: got %v", err)
	}
	_, _, gotLate := rb.Stats()
	if gotLate != 1 {
		t.Fatalf("late count = %d, want 1", gotLate)
	}
}

func TestFarPacketSlidesWindowAndReportsGap(t *testing.T) {
	rb, _ := NewReorderBuffer(4)
	_ = rb.Push(pkt(0, 0, 0))
	rb.PopAll() // 锚定并交付基准序号 0
	// 远侧包 10：新基准 = 10-4+1 = 7，序号 1..6 滑出窗口定性为缺口。
	if err := rb.Push(pkt(10, 1600, 10)); err != nil {
		t.Fatalf("far packet should be retained: %v", err)
	}
	gaps := rb.Gaps()
	if len(gaps) != 1 || gaps[0].SeqStart != 1 || gaps[0].SeqEnd != 6 ||
		!gaps[0].HasNext || gaps[0].NextTS != 1600 {
		t.Fatalf("slide gap wrong: %+v", gaps)
	}
	// 滑出后的 3 再到达，判窗口外迟到。
	if err := rb.Push(pkt(3, 480, 3)); !errors.Is(err, ErrLateOutside) {
		t.Fatalf("late after slide: got %v", err)
	}
	out := rb.Flush()
	if len(out) != 1 || out[0].SequenceNumber != 10 {
		t.Fatalf("far packet delivery: %v", out)
	}
}

func TestSequenceWraparound(t *testing.T) {
	rb, _ := NewReorderBuffer(8)
	// 先让 65535 成为基准，再让回绕后的 0/1 入窗。
	_ = rb.Push(pkt(65535, 90, 255))
	if err := rb.Push(pkt(0, 100, 0)); err != nil {
		t.Fatalf("wrapped successor rejected: %v", err)
	}
	// 65535 与 0 均在窗口内且连续，应一起按序交付。
	if got := rb.PopAll(); len(got) != 2 ||
		got[0].SequenceNumber != 65535 || got[1].SequenceNumber != 0 {
		t.Fatalf("wrapped in-order delivery: %v", got)
	}
	_ = rb.Push(pkt(1, 110, 1))
	// 65534 是当前基准 1 的前两个序号：判窗口外迟到。
	if err := rb.Push(pkt(65534, 80, 254)); !errors.Is(err, ErrLateOutside) {
		t.Fatalf("predecessor after wrap must be late, got %v", err)
	}
	out := rb.PopAll()
	want := []uint16{1}
	if len(out) != len(want) {
		t.Fatalf("wrapped delivery len=%d, want %d", len(out), len(want))
	}
	for i, p := range out {
		if p.SequenceNumber != want[i] {
			t.Fatalf("wrap order[%d]=%d, want %d", i, p.SequenceNumber, want[i])
		}
	}
}

func TestFlushReportsTrailingGap(t *testing.T) {
	rb, _ := NewReorderBuffer(16)
	_ = rb.Push(pkt(3, 300, 3))
	// 4,5,6 缺失且无后续包：调用方知道流的总序号空间，显式标记流尾缺口。
	out := rb.Flush()
	if len(out) != 1 || out[0].SequenceNumber != 3 {
		t.Fatalf("flush order: %v", out)
	}
	rb.MarkTrailingHole(6)
	gaps := rb.Gaps()
	if len(gaps) != 1 || gaps[0].SeqStart != 4 || gaps[0].SeqEnd != 6 || gaps[0].HasNext {
		t.Fatalf("flush gap wrong: %+v", gaps)
	}
}

func TestPeakBuffered(t *testing.T) {
	rb, _ := NewReorderBuffer(16)
	for _, s := range []uint16{0, 2, 3, 4, 5} { // 1 缺失
		_ = rb.Push(pkt(s, uint32(s)*160, byte(s)))
	}
	if rb.PeakBuffered() != 5 {
		t.Fatalf("peak=%d, want 5", rb.PeakBuffered())
	}
	rb.PopAll() // 只交付 0
	if rb.PeakBuffered() != 5 {
		t.Fatal("peak must remain a high-water mark")
	}
}
