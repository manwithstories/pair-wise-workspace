package rtp

import (
	"testing"

	"archive/internal/model"
)

func pkt(ssrc uint32, seq uint16) model.Packet {
	return model.Packet{SSRC: ssrc, Sequence: seq, Timestamp: uint32(seq) * 100}
}

// TestRepairInOrder verifies a clean stream emits every packet in order.
func TestRepairInOrder(t *testing.T) {
	r := NewRepairer(8)
	var got []int64
	r.Emit = func(p model.Packet) { got = append(got, p.ExtSequence) }
	for i := uint16(0); i < 100; i++ {
		r.Feed(pkt(1, i))
	}
	r.Flush()
	if len(got) != 100 {
		t.Fatalf("emitted %d, want 100", len(got))
	}
	for i, v := range got {
		if v != int64(i) {
			t.Fatalf("out of order at %d: %d", i, v)
		}
	}
}

// TestRepairReorderWithinWindow verifies late packets inside the window are
// recovered and emitted in sequence order.
func TestRepairReorderWithinWindow(t *testing.T) {
	r := NewRepairer(8)
	var got []int64
	r.Emit = func(p model.Packet) { got = append(got, p.ExtSequence) }
	r.Feed(pkt(1, 0))
	r.Feed(pkt(1, 2)) // 1 missing, held within window
	r.Feed(pkt(1, 1)) // late arrival fills the hole
	for i := uint16(3); i < 20; i++ {
		r.Feed(pkt(1, i))
	}
	r.Flush()
	if len(got) != 20 {
		t.Fatalf("emitted %d, want 20", len(got))
	}
	for i, v := range got {
		if v != int64(i) {
			t.Fatalf("out of order at %d: %d", i, v)
		}
	}
	if r.Stats().Reordered == 0 {
		t.Error("expected reordered count > 0")
	}
}

// TestRepairDuplicates verifies same-SSRC same-sequence packets dedup.
func TestRepairDuplicates(t *testing.T) {
	r := NewRepairer(8)
	n := 0
	r.Emit = func(model.Packet) { n++ }
	r.Feed(pkt(1, 0))
	r.Feed(pkt(1, 0)) // exact duplicate
	r.Feed(pkt(1, 1))
	r.Feed(pkt(1, 1))
	r.Flush()
	if n != 2 {
		t.Fatalf("emitted %d unique, want 2", n)
	}
	if d := r.Stats().Duplicates; d != 2 {
		t.Errorf("duplicates = %d, want 2", d)
	}
}

// TestRepairBurstLossBeyondWindow verifies a gap wider than the window is
// declared a traceable loss rather than buffered forever.
func TestRepairBurstLossBeyondWindow(t *testing.T) {
	r := NewRepairer(4)
	r.Emit = func(model.Packet) {}
	r.Feed(pkt(1, 0))
	// packets 1..6 missing; feed 7 onward to push past the window
	for i := uint16(7); i < 20; i++ {
		r.Feed(pkt(1, i))
	}
	r.Flush()
	st := r.Stats()
	if st.Lost == 0 {
		t.Fatal("expected lost packets beyond window")
	}
	if len(st.LostRanges) == 0 {
		t.Fatal("expected a traceable lost range")
	}
}

// TestRepairSequenceWrap verifies 16-bit wrap extends to monotonic int64.
func TestRepairSequenceWrap(t *testing.T) {
	r := NewRepairer(8)
	var got []int64
	r.Emit = func(p model.Packet) { got = append(got, p.ExtSequence) }
	// near the 16-bit boundary
	for seq := uint16(0xfff8); seq != 0x0008; seq++ {
		r.Feed(pkt(1, seq))
	}
	r.Flush()
	if len(got) != 16 {
		t.Fatalf("emitted %d, want 16", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[i-1]+1 {
			t.Fatalf("wrap not monotonic at %d: %d -> %d", i, got[i-1], got[i])
		}
	}
}

// TestRepairMultiSSRCInterleave verifies interleaved streams do not cause
// false losses (regression for the per-stream window bug).
func TestRepairMultiSSRCInterleave(t *testing.T) {
	r := NewRepairer(16)
	counts := map[uint32]int{}
	r.Emit = func(p model.Packet) { counts[p.SSRC]++ }
	const n = 500
	for i := 0; i < n; i++ {
		r.Feed(pkt(100, uint16(i)))
		r.Feed(pkt(200, uint16(i)))
	}
	r.Flush()
	if counts[100] != n || counts[200] != n {
		t.Fatalf("per-stream emitted %d/%d, want %d each", counts[100], counts[200], n)
	}
	if lost := r.Stats().Lost; lost != 0 {
		t.Errorf("false losses on clean interleaved streams: %d", lost)
	}
}

// TestRepairBoundedBuffer verifies the reorder buffer never exceeds the
// configured window regardless of input length.
func TestRepairBoundedBuffer(t *testing.T) {
	r := NewRepairer(8)
	r.Emit = func(model.Packet) {}
	// adversarial: always advance far ahead, leaving holes
	for i := uint16(0); i < 1000; i += 2 {
		r.Feed(pkt(1, i))
	}
	r.Flush()
	if peak := r.Stats().PeakBuffer; peak > 8 {
		t.Errorf("peak buffer %d exceeds window 8", peak)
	}
}
