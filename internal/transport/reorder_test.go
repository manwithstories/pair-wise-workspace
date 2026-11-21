package transport

import (
	"testing"

	"github.com/pion/rtp"
)

func mkPacket(seq uint16, ts uint32, size int) rtp.Packet {
	return rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: 77, PayloadType: 96},
		Payload: make([]byte, size),
	}
}

func pushAll(t *testing.T, r *Reorderer, seqs []uint16) {
	t.Helper()
	for i, s := range seqs {
		if _, ok, err := r.PushPacket(mkPacket(s, uint32(i)*3000, 100), int64(i)); err != nil {
			t.Fatalf("push %d: %v", s, err)
		} else if !ok {
			t.Fatalf("packet %d refused unexpectedly", s)
		}
	}
}

// pushSeq pushes one packet and returns whether it was accepted, without
// failing the test on refusal (used for duplicate/late cases).
func pushSeq(t *testing.T, r *Reorderer, seq uint16, arrival int64) bool {
	t.Helper()
	_, ok, err := r.PushPacket(mkPacket(seq, uint32(arrival)*3000, 100), arrival)
	if err != nil {
		t.Fatalf("push %d: %v", seq, err)
	}
	return ok
}

func TestExtendWrap(t *testing.T) {
	r := NewReorderer(DefaultReorderConfig())
	if got := r.Extend(65530); got != 65530 {
		t.Fatalf("first = %d", got)
	}
	if got := r.Extend(65534); got != 65534 {
		t.Fatalf("got %d", got)
	}
	if got := r.Extend(2); got != 65538 {
		t.Fatalf("post-wrap = %d", got)
	}
	if !r.wrapped {
		t.Fatal("wrap not recorded")
	}
	// A stale packet from before the wrap maps back into the old epoch.
	if got := r.Extend(65533); got != 65533 {
		t.Fatalf("stale epoch = %d", got)
	}
}

func TestInOrderDelivery(t *testing.T) {
	r := NewReorderer(ReorderConfig{Window: 8, StartupHold: 2, BurstGapThreshold: 4})
	seqs := make([]uint16, 20)
	for i := range seqs {
		seqs[i] = uint16(i)
	}
	pushAll(t, r, seqs)
	tail, stats := r.Stats()
	got := append(r.Drain(), tail...)
	if len(got) != len(seqs) {
		t.Fatalf("delivered %d, want %d", len(got), len(seqs))
	}
	for i := 1; i < len(got); i++ {
		if got[i].ExtSeq <= got[i-1].ExtSeq {
			t.Fatalf("out of order at %d: %d then %d", i, got[i-1].ExtSeq, got[i].ExtSeq)
		}
	}
	if stats.Duplicates != 0 || stats.Lost != 0 || stats.Reordered != 0 || len(stats.Gaps) != 0 {
		t.Fatalf("unexpected stats %+v", stats)
	}
}

func TestDuplicateIsCountedNotDelivered(t *testing.T) {
	r := NewReorderer(ReorderConfig{Window: 8, StartupHold: 2, BurstGapThreshold: 4})
	pushAll(t, r, []uint16{0, 1, 2, 3, 4, 5, 6, 7})
	if pushSeq(t, r, 2, 8) || pushSeq(t, r, 3, 9) {
		t.Fatal("duplicate sequences were accepted")
	}
	_, stats := r.Stats()
	if stats.Duplicates != 2 {
		t.Fatalf("duplicates = %d, want 2", stats.Duplicates)
	}
	if stats.Unique != 8 {
		t.Fatalf("unique = %d, want 8", stats.Unique)
	}
}

func TestReorderWithinWindow(t *testing.T) {
	r := NewReorderer(ReorderConfig{Window: 32, StartupHold: 2, BurstGapThreshold: 4})
	// Arrival order deliberately scrambled; all 10 sequences are present.
	pushAll(t, r, []uint16{0, 2, 1, 4, 3, 6, 5, 9, 8, 7})
	_, stats := r.Stats()
	if stats.Reordered == 0 {
		t.Fatal("reordering not recorded")
	}
	if stats.Lost != 0 || len(stats.Gaps) != 0 {
		t.Fatalf("reordered stream reported loss: %+v", stats)
	}
	if stats.Unique != 10 {
		t.Fatalf("unique = %d", stats.Unique)
	}
}

func TestBurstLossCreatesExplicitGap(t *testing.T) {
	r := NewReorderer(ReorderConfig{Window: 8, StartupHold: 2, BurstGapThreshold: 4})
	// Missing 4,5,6,7 (a 4-packet burst).
	pushAll(t, r, []uint16{0, 1, 2, 3, 8, 9, 10, 11})
	_, stats := r.Stats()
	gaps := stats.BlockedGaps()
	if len(gaps) != 1 {
		t.Fatalf("gaps = %+v, want one blocked gap", stats.Gaps)
	}
	if gaps[0].StartExt != 4 || gaps[0].Count != 4 || !gaps[0].Blocked {
		t.Fatalf("gap = %+v", gaps[0])
	}
	if stats.Lost != 4 {
		t.Fatalf("lost = %d, want 4", stats.Lost)
	}
}

func TestShortGapDoesNotBlock(t *testing.T) {
	r := NewReorderer(ReorderConfig{Window: 8, StartupHold: 2, BurstGapThreshold: 4})
	pushAll(t, r, []uint16{0, 1, 2, 4, 5, 6, 7})
	_, stats := r.Stats()
	if len(stats.Gaps) != 1 || stats.Gaps[0].Count != 1 || stats.Gaps[0].Blocked {
		t.Fatalf("short gap mishandled: %+v", stats.Gaps)
	}
	if stats.Lost != 1 {
		t.Fatalf("lost = %d", stats.Lost)
	}
}

func TestLatePacketCannotOverwriteCommitted(t *testing.T) {
	r := NewReorderer(ReorderConfig{Window: 4, StartupHold: 2, BurstGapThreshold: 4})
	pushAll(t, r, []uint16{0, 1, 2, 3, 8, 9, 10, 11, 12, 13})
	// Deliver a committed interval downstream.
	delivered := r.Drain()
	if len(delivered) == 0 {
		t.Fatal("nothing delivered before late push")
	}
	// Sequences 4 and 5 are inside the frozen gap and were never seen; both
	// must be refused without overwriting the committed output.
	if pushSeq(t, r, 4, 99) || pushSeq(t, r, 5, 100) {
		t.Fatal("packet in committed gap was accepted")
	}
	_, stats := r.Stats()
	if stats.Late < 2 {
		t.Fatalf("late = %d, want >= 2", stats.Late)
	}
	// Re-draining must not contain the late packet.
	for _, p := range r.Drain() {
		if p.ExtSeq == 4 || p.ExtSeq == 1 {
			t.Fatalf("late packet %d leaked into output", p.ExtSeq)
		}
	}
}

func TestWindowOverflowFreezesHole(t *testing.T) {
	r := NewReorderer(ReorderConfig{Window: 2, StartupHold: 2, BurstGapThreshold: 4})
	// Sequence 2..9 arrive with 0,1; buffer must not grow unbounded.
	pushAll(t, r, []uint16{0, 5, 6, 7, 8, 9, 10, 11, 12, 13})
	_, stats := r.Stats()
	if len(stats.Gaps) == 0 {
		t.Fatal("window overflow did not freeze a gap")
	}
	if stats.Gaps[0].StartExt != 1 || stats.Gaps[0].Count != 4 || !stats.Gaps[0].Blocked {
		t.Fatalf("frozen gap = %+v", stats.Gaps[0])
	}
}

func TestStartupOutOfOrderNotLate(t *testing.T) {
	r := NewReorderer(ReorderConfig{Window: 32, StartupHold: 8, BurstGapThreshold: 4})
	pushAll(t, r, []uint16{3, 1, 0, 2, 5, 4, 7, 6, 9, 8})
	_, stats := r.Stats()
	if stats.Late != 0 {
		t.Fatalf("startup reorder misclassified as late: %+v", stats)
	}
	if stats.Unique != 10 {
		t.Fatalf("unique = %d", stats.Unique)
	}
}
