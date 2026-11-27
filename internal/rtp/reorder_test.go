package rtp

import (
	"testing"
)

func mkPkt(ssrc uint32, seq uint16) *Packet {
	return &Packet{SSRC: ssrc, Seq: seq, Payload: []byte{byte(seq)}}
}

func seqsOf(pkts []*Packet) []uint16 {
	var out []uint16
	for _, p := range pkts {
		out = append(out, p.Seq)
	}
	return out
}

func TestReorderInOrder(t *testing.T) {
	r := NewReorderer(8)
	var got []uint16
	for i := uint16(0); i < 5; i++ {
		em, err := r.Push(mkPkt(1, i))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, seqsOf(em)...)
	}
	got = append(got, seqsOf(r.Flush())...)
	if len(got) != 5 {
		t.Fatalf("got %v", got)
	}
	for i, s := range got {
		if s != uint16(i) {
			t.Fatalf("order %v", got)
		}
	}
	if st := r.Stats(); st.Lost != 0 || st.Duplicates != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestReorderOutOfOrderAndLoss(t *testing.T) {
	r := NewReorderer(4)
	// 0,2,1 then gap at 3 declared lost once window slides past.
	var got []uint16
	push := func(seq uint16) {
		em, err := r.Push(mkPkt(1, seq))
		if err != nil {
			t.Fatalf("seq %d: %v", seq, err)
		}
		got = append(got, seqsOf(em)...)
	}
	push(0)
	push(2)
	push(1)
	push(4)
	push(5)
	push(6) // window [3,6]: 3 declared lost
	got = append(got, seqsOf(r.Flush())...)
	want := []uint16{0, 1, 2, 4, 5, 6}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	st := r.Stats()
	if st.Lost != 1 || st.Reordered != 2 {
		t.Fatalf("stats %+v", st)
	}
}

func TestReorderWrapAndDuplicate(t *testing.T) {
	r := NewReorderer(8)
	var got []uint16
	push := func(seq uint16) {
		em, err := r.Push(mkPkt(1, seq))
		if err != nil {
			t.Fatalf("seq %d: %v", seq, err)
		}
		got = append(got, seqsOf(em)...)
	}
	// Start near the 16-bit wrap boundary.
	push(65534)
	push(65535)
	push(0) // wraps to ext 65536
	push(1)
	push(1) // duplicate: idempotent drop
	push(2)
	got = append(got, seqsOf(r.Flush())...)
	want := []uint16{65534, 65535, 0, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if st := r.Stats(); st.Duplicates != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestReorderOutOfWindowRecorded(t *testing.T) {
	r := NewReorderer(4)
	for i := uint16(10); i < 20; i++ {
		if _, err := r.Push(mkPkt(1, i)); err != nil {
			t.Fatal(err)
		}
	}
	// ext 10 is far behind the window trailing edge now.
	_, perr := r.Push(mkPkt(1, 10))
	if perr == nil {
		t.Fatal("expected out-of-window error record")
	}
	if st := r.Stats(); st.OutOfWindow != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestExtendSeqChoosesNearestCycle(t *testing.T) {
	if got := extendSeq(0, 65535); got != 65536 {
		t.Fatalf("extendSeq(0, 65535) = %d", got)
	}
	if got := extendSeq(65534, 65536); got != 65534 {
		t.Fatalf("extendSeq(65534, 65536) = %d", got)
	}
}
