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
	r := NewRecoverer(1)               // window 16
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

// A late leading packet inside the warm-up window moves the stream origin
// back instead of being dropped or inflating the loss count.
func TestLateLeadingPacket(t *testing.T) {
	r := NewRecoverer(1) // window 16
	feedAll(t, r, 5, 7, 3, 4, 6, 8)
	st := r.Stats()
	if st.Emitted != 6 || st.Lost != 0 || st.Late != 0 {
		t.Fatalf("stats %+v", st)
	}
	got := outSeqs(r)
	for i, want := range []uint16{3, 4, 5, 6, 7, 8} {
		if got[i] != want {
			t.Fatalf("out %v", got)
		}
	}
}

// A stream that starts just before a 16-bit wrap and continues through it
// must not report a 64k-packet hole.
func TestStartNearWrap(t *testing.T) {
	r := NewRecoverer(1)
	seqs := make([]uint16, 0, 12)
	for s := uint16(0xfffa); ; s++ {
		seqs = append(seqs, s)
		if s == 0x0005 {
			break
		}
	}
	feedAll(t, r, seqs...)
	st := r.Stats()
	if st.Emitted != 12 || st.Lost != 0 {
		t.Fatalf("stats %+v", st)
	}
	got := outSeqs(r)
	if got[0] != 0xfffa || got[11] != 0x0005 {
		t.Fatalf("out %v", got)
	}
}

// A declared reorder depth raises the window floor even at low bandwidth.
func TestReorderDepthFloor(t *testing.T) {
	r := NewRecoverer(0.5) // window 8
	r.SetReorderDepth(40)
	if r.Window() != 40 {
		t.Fatalf("window %d", r.Window())
	}
	r.SetBandwidth(8) // 64 > floor: bandwidth wins
	if r.Window() != 64 {
		t.Fatalf("window %d", r.Window())
	}
	r.SetBandwidth(1) // 16 < floor: floor wins
	if r.Window() != 40 {
		t.Fatalf("window %d", r.Window())
	}
}

// A packet far behind the emission cursor (history already pruned) is
// counted as late, and does not disturb the emitted stream.
func TestLateAfterClose(t *testing.T) {
	r := NewRecoverer(1) // window 16, prune keeps 32 seqs of history
	for s := 0; s <= 200; s++ {
		if err := r.Feed("a", packet(uint16(s))); err != nil {
			t.Fatal(err)
		}
	}
	before := r.stats.Emitted
	if err := r.Feed("a", packet(0)); err != nil {
		t.Fatal(err)
	}
	r.Flush()
	st := r.Stats()
	if st.Emitted != before || st.Late != 1 || st.Lost != 0 {
		t.Fatalf("stats %+v", st)
	}
	if st.Received != 202 {
		t.Fatalf("received %d", st.Received)
	}
}
