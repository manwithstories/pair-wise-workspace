package rtp

import (
	"testing"

	"github.com/pion/rtp"

	"mediad/internal/fault"
)

func pkt(ssrc uint32, seq uint16, payload ...byte) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{SSRC: ssrc, SequenceNumber: seq},
		Payload: payload,
	}
}

func drainAll(r *Reorderer, pkts ...*rtp.Packet) (out []*rtp.Packet, errs []error) {
	for _, p := range pkts {
		o, err := r.Push(p)
		out = append(out, o...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return append(out, r.Flush()...), errs
}

func seqs(pkts []*rtp.Packet) []uint16 {
	var s []uint16
	for _, p := range pkts {
		s = append(s, p.SequenceNumber)
	}
	return s
}

func TestReorderWithinWindow(t *testing.T) {
	r := New(4, 16)
	out, _ := drainAll(r, pkt(1, 0), pkt(1, 2), pkt(1, 1), pkt(1, 3))
	got := seqs(out)
	want := []uint16{0, 1, 2, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
	if len(r.Stats().Gaps) != 0 {
		t.Fatalf("unexpected gaps: %+v", r.Stats().Gaps)
	}
}

func TestDuplicateEliminated(t *testing.T) {
	r := New(4, 16)
	out, errs := drainAll(r, pkt(1, 0, 0xA), pkt(1, 0, 0xA), pkt(1, 1, 0xB))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(out) != 2 {
		t.Fatalf("got %d packets, want 2 (duplicate dropped)", len(out))
	}
	if r.Stats().Duplicates != 1 {
		t.Fatalf("duplicates=%d, want 1", r.Stats().Duplicates)
	}
}

func TestConflictQuarantined(t *testing.T) {
	r := New(4, 16)
	_, errs := drainAll(r, pkt(1, 0, 0xA), pkt(1, 0, 0xB))
	if len(errs) != 1 || fault.KindOf(errs[0]) != fault.KindConflict {
		t.Fatalf("want conflict error, got %v", errs)
	}
	if len(r.Stats().Conflicts) != 1 || r.Stats().Conflicts[0].Seq != 0 {
		t.Fatalf("conflicts=%+v", r.Stats().Conflicts)
	}
}

func TestBurstLossFormsExplicitGap(t *testing.T) {
	r := New(2, 16)
	// seq 1..3 lost in a burst; 4 arrives and overflows the window.
	out, _ := drainAll(r, pkt(1, 0), pkt(1, 4), pkt(1, 5), pkt(1, 6))
	if got := seqs(out); len(got) != 4 || got[1] != 4 {
		t.Fatalf("order %v, want 0,4,5,6 with gap at 1..3", got)
	}
	merged := MergeGaps(r.Stats().Gaps)
	if len(merged) != 1 || merged[0].Start != 1 || merged[0].Count != 3 {
		t.Fatalf("gaps=%+v, want 1..3", merged)
	}
}

func TestLatePacketUnrecoverable(t *testing.T) {
	r := New(1, 16)
	// 1 is declared a gap once the window closes, then arrives too late.
	out, _ := drainAll(r, pkt(1, 0), pkt(1, 2), pkt(1, 3), pkt(1, 1))
	if got := seqs(out); len(got) != 3 || got[1] != 2 {
		t.Fatalf("order %v, late packet must not be emitted", got)
	}
	st := r.Stats()
	if st.Late != 1 {
		t.Fatalf("late=%d, want 1", st.Late)
	}
	for _, g := range st.Gaps {
		if g.Start == 1 && g.Recoverable {
			t.Fatal("gap for late packet must be unrecoverable")
		}
	}
}

func TestAdaptiveWindow(t *testing.T) {
	r := New(2, 8)
	r.AdjustForBandwidth(1_000_000, 1_000_000)
	if r.Window() != 2 {
		t.Fatalf("window=%d, want base 2 at reference bandwidth", r.Window())
	}
	r.AdjustForBandwidth(250_000, 1_000_000) // 4x weaker link
	if r.Window() != 8 {
		t.Fatalf("window=%d, want 8 (capped)", r.Window())
	}
	r.AdjustForBandwidth(500_000, 1_000_000)
	if r.Window() != 4 {
		t.Fatalf("window=%d, want 4", r.Window())
	}
}
