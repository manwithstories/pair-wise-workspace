package rtp

import (
	"testing"

	"github.com/pion/rtp"
)

func packet(seq uint16) []byte {
	p := rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 900}, Payload: []byte{byte(seq)}}
	raw, err := p.Marshal()
	if err != nil {
		panic(err)
	}
	return raw
}

func feedAll(t *testing.T, r *Recoverer, seqs ...uint16) {
	t.Helper()
	for _, s := range seqs {
		if err := r.Feed("a", packet(s)); err != nil {
			t.Fatal(err)
		}
	}
	r.Flush()
}

func outSeqs(r *Recoverer) []uint16 {
	var out []uint16
	for _, p := range r.Out {
		out = append(out, p.Header.SequenceNumber)
	}
	return out
}

func TestInOrder(t *testing.T) {
	r := NewRecoverer(1)
	feedAll(t, r, 0, 1, 2, 3)
	st := r.Stats()
	if st.Emitted != 4 || st.Lost != 0 || st.Reordered != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestReorderAndDuplicate(t *testing.T) {
	r := NewRecoverer(1) // window 16
	feedAll(t, r, 0, 2, 3, 1, 2, 4, 5) // 1 delayed, 2 duplicated
	st := r.Stats()
	if st.Emitted != 6 || st.Duplicates != 1 || st.Lost != 0 {
		t.Fatalf("stats %+v", st)
	}
	got := outSeqs(r)
	for i, want := range []uint16{0, 1, 2, 3, 4, 5} {
		if got[i] != want {
			t.Fatalf("out %v", got)
		}
	}
}

func TestBurstLossGoesToRetryableGaps(t *testing.T) {
	r := NewRecoverer(1)
	// lose 10..13 (burst of 4), stream continues
	feedAll(t, r, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25)
	st := r.Stats()
	if st.Lost != 4 || len(st.Gaps) != 1 || st.Gaps[0].Len() != 4 {
		t.Fatalf("stats %+v", st)
	}
	if st.Gaps[0].Start != 10 || st.Gaps[0].End != 13 {
		t.Fatalf("gap %+v", st.Gaps[0])
	}
	want := float64(22) / 26
	if st.RecoveryRate() < want-1e-9 {
		t.Fatalf("rate %f want %f", st.RecoveryRate(), want)
	}
}

func TestSequenceWrap(t *testing.T) {
	r := NewRecoverer(1)
	feedAll(t, r, 0xfffe, 0xffff, 0x0000, 0x0001)
	st := r.Stats()
	if st.Emitted != 4 || st.Lost != 0 {
		t.Fatalf("stats %+v", st)
	}
	got := outSeqs(r)
	want := []uint16{0xfffe, 0xffff, 0, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("out %v", got)
		}
	}
}

func TestWindowAdaptsToBandwidth(t *testing.T) {
	r := NewRecoverer(1)
	if r.Window() != 16 {
		t.Fatalf("window %d", r.Window())
	}
	r.SetBandwidth(8)
	if r.Window() != 64 {
		t.Fatalf("window %d", r.Window())
	}
	r.SetBandwidth(0.1)
	if r.Window() != 8 {
		t.Fatalf("window %d", r.Window())
	}
}
