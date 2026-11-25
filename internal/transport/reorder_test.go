package transport

import (
	"testing"

	"github.com/pion/rtp"
)

func pkt(seq uint16, ts uint32) *rtp.Packet {
	p := &rtp.Packet{}
	p.SequenceNumber = seq
	p.Timestamp = ts
	return p
}

func seqs(ps []*rtp.Packet) []uint16 {
	out := make([]uint16, len(ps))
	for i, p := range ps {
		out[i] = p.SequenceNumber
	}
	return out
}

func TestInOrderStream(t *testing.T) {
	ordered, gaps, st, f := ReorderAll([]*rtp.Packet{
		pkt(0, 0), pkt(1, 960), pkt(2, 1920), pkt(3, 2880),
	}, 8)
	if f != nil {
		t.Fatal(f)
	}
	if got := seqs(ordered); len(got) != 4 || got[0] != 0 || got[3] != 3 {
		t.Fatalf("order=%v", got)
	}
	if len(gaps) != 0 || st.Emitted != 4 {
		t.Fatalf("gaps=%v stats=%+v", gaps, st)
	}
}

func TestDedupBySeqAndTimestamp(t *testing.T) {
	rb, _ := NewReorderBuffer(8)
	var all []*rtp.Packet
	for _, p := range []*rtp.Packet{pkt(5, 500), pkt(5, 500), pkt(6, 600)} {
		out, f := rb.Push(p)
		if f != nil {
			t.Fatal(f)
		}
		all = append(all, out...)
	}
	all = append(all, rb.Flush()...)
	if rb.Stats().Duplicates != 1 {
		t.Fatalf("duplicates=%d want 1", rb.Stats().Duplicates)
	}
	if len(all) != 2 {
		t.Fatalf("emitted=%d want 2", len(all))
	}
}

func TestReorderWithinWindow(t *testing.T) {
	ordered, _, st, f := ReorderAll([]*rtp.Packet{
		pkt(0, 0), pkt(2, 1920), pkt(1, 960), pkt(3, 2880),
	}, 4)
	if f != nil {
		t.Fatal(f)
	}
	if got := seqs(ordered); len(got) != 4 || got[1] != 1 || got[2] != 2 {
		t.Fatalf("not reordered correctly: %v", got)
	}
	if st.Reordered == 0 {
		t.Fatal("expected reorder stat")
	}
}

func TestGapDetectedForBurstLoss(t *testing.T) {
	// seq 1..4 missing between 0 and 5, window 2 ages them out.
	ordered, gaps, _, f := ReorderAll([]*rtp.Packet{pkt(0, 0), pkt(5, 4800)}, 2)
	if f != nil {
		t.Fatal(f)
	}
	if got := seqs(ordered); len(got) != 2 || got[1] != 5 {
		t.Fatalf("ordered=%v", got)
	}
	if len(gaps) != 1 || gaps[0].FromSeq != 1 || gaps[0].ToSeq != 4 || gaps[0].Packets != 4 {
		t.Fatalf("gaps=%+v", gaps)
	}
}

func TestLatePacketBeyondWindowNotEmitted(t *testing.T) {
	rb, _ := NewReorderBuffer(2)
	var emitted []uint16
	push := func(p *rtp.Packet) {
		out, f := rb.Push(p)
		if f != nil {
			t.Fatal(f)
		}
		for _, q := range out {
			emitted = append(emitted, q.SequenceNumber)
		}
	}
	push(pkt(10, 10000))
	push(pkt(13, 13000))
	push(pkt(14, 14000))
	// seq 11 arrives far too late and with a fresh timestamp (new dedup key).
	push(pkt(11, 11001))
	out := rb.Flush()
	for _, q := range out {
		emitted = append(emitted, q.SequenceNumber)
	}
	for _, s := range emitted {
		if s == 11 {
			t.Fatalf("late seq 11 must not be emitted: %v", emitted)
		}
	}
	if rb.Stats().LateDropped == 0 {
		t.Fatal("expected late drop stat")
	}
	// An exact duplicate of an already emitted packet is also never re-emitted.
	before := len(emitted)
	push(pkt(10, 10000))
	if len(emitted) != before {
		t.Fatal("duplicate emitted twice")
	}
}

func TestSequenceWrap(t *testing.T) {
	ordered, gaps, st, f := ReorderAll([]*rtp.Packet{
		pkt(65534, 900), pkt(65535, 1860), pkt(0, 2820), pkt(1, 3780),
	}, 8)
	if f != nil {
		t.Fatal(f)
	}
	got := seqs(ordered)
	if len(got) != 4 || got[2] != 0 || got[3] != 1 {
		t.Fatalf("wrap order wrong: %v gaps=%v", got, gaps)
	}
	if len(gaps) != 0 || st.Wraps == 0 {
		t.Fatalf("expected one wrap and no gaps, gaps=%v wraps=%d", gaps, st.Wraps)
	}
}

func TestNilPacketRejected(t *testing.T) {
	rb, _ := NewReorderBuffer(4)
	if _, f := rb.Push(nil); f == nil {
		t.Fatal("nil packet must fault")
	}
	if _, f := NewReorderBuffer(-1); f == nil {
		t.Fatal("negative window must fault")
	}
}
