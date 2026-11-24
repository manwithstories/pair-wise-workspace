package rtp

import (
	"errors"
	"testing"

	"mediarun/internal/model"
)

func pkt(seq uint16) Packet { return Packet{Sequence: seq, Timestamp: uint32(seq) * 3000} }

func seqs(ps []Packet) []uint16 {
	out := make([]uint16, len(ps))
	for i, p := range ps {
		out[i] = p.Sequence
	}
	return out
}

func eq(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNewRejectsBadWindow(t *testing.T) {
	for _, w := range []int{0, -1, MaxWindow + 1} {
		if _, err := New(1, w); !errors.Is(err, model.ErrInvalid) {
			t.Fatalf("window %d: %v", w, err)
		}
	}
	if _, err := New(1, 8); err != nil {
		t.Fatal(err)
	}
}

func TestInOrderPassthrough(t *testing.T) {
	r, _ := New(1, 8)
	var got []uint16
	for i := uint16(0); i < 10; i++ {
		res := r.Push(pkt(i))
		got = append(got, seqs(res.Ordered)...)
		if len(res.Gaps) != 0 {
			t.Fatalf("unexpected gap at %d", i)
		}
	}
	want := []uint16{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	if !eq(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestReorderedDelivery(t *testing.T) {
	r, _ := New(1, 8)
	var got []uint16
	for _, s := range []uint16{0, 2, 1, 4, 3, 5} {
		res := r.Push(pkt(s))
		got = append(got, seqs(res.Ordered)...)
	}
	got = append(got, seqs(r.Close().Ordered)...)
	want := []uint16{0, 1, 2, 3, 4, 5}
	if !eq(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestDuplicateSuppressed(t *testing.T) {
	r, _ := New(1, 8)
	r.Push(pkt(10))
	res := r.Push(pkt(10))
	if !res.Duplicate || len(res.Ordered) != 0 {
		t.Fatalf("duplicate not suppressed: %+v", res)
	}
	// Late duplicate after it was delivered.
	r.Push(pkt(11))
	res = r.Push(pkt(10))
	if !res.Duplicate {
		t.Fatalf("late duplicate: %+v", res)
	}
	if st := r.Stats(); st.Duplicates != 2 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestWindowOverflowRegistersGap(t *testing.T) {
	r, _ := New(1, 3)
	var got []uint16
	var gaps []Gap
	got = append(got, seqs(r.Push(pkt(0)).Ordered)...)
	// Three packets past the hole fill the window exactly; the next
	// observation forces the buffer to give up on 1..4.
	for _, s := range []uint16{5, 6, 7} {
		res := r.Push(pkt(s))
		got = append(got, seqs(res.Ordered)...)
		gaps = append(gaps, res.Gaps...)
	}
	if !eq(got, []uint16{0, 5, 6, 7}) {
		t.Fatalf("delivered %v", got)
	}
	if len(gaps) != 1 {
		t.Fatalf("gaps: %+v", gaps)
	}
	g := gaps[0]
	if g.From != 1 || g.To != 4 || g.Count != 4 {
		t.Fatalf("gap range: %+v", g)
	}
	// The late packets can no longer be placed; they must be reported,
	// never silently inserted into the delivered stream.
	for _, s := range []uint16{2, 3} {
		res := r.Push(pkt(s))
		if len(res.Ordered) != 0 || !res.TooOld {
			t.Fatalf("late %d: %+v", s, res)
		}
	}
	res := r.Close()
	if len(res.Ordered) != 0 || len(res.Gaps) != 0 {
		t.Fatalf("close after overflow: %+v", res)
	}
}

func TestCloseReportsTrailingHole(t *testing.T) {
	r, _ := New(7, 8)
	r.Push(pkt(1))
	r.Push(pkt(3))
	if st := r.Stats(); st.Delivered != 1 {
		t.Fatalf("delivered %d", st.Delivered)
	}
	res := r.Close()
	if !eq(seqs(res.Ordered), []uint16{3}) {
		t.Fatalf("ordered %v", seqs(res.Ordered))
	}
	if len(res.Gaps) != 1 || res.Gaps[0].From != 2 || res.Gaps[0].To != 2 {
		t.Fatalf("gaps %+v", res.Gaps)
	}
	if st := r.Stats(); st.GapPackets != 1 || st.Delivered != 2 {
		t.Fatalf("stats %+v", st)
	}
}

func TestSequenceWraparound(t *testing.T) {
	r, _ := New(1, 8)
	base := uint16(65532)
	var got []uint16
	for i := uint16(0); i < 8; i++ {
		res := r.Push(pkt(base + i))
		got = append(got, seqs(res.Ordered)...)
	}
	want := []uint16{65532, 65533, 65534, 65535, 0, 1, 2, 3}
	if !eq(got, want) {
		t.Fatalf("wrap delivery %v", got)
	}
	if len(r.Close().Gaps) != 0 {
		t.Fatal("wrap must not create a gap")
	}
}

func TestTooOldRejected(t *testing.T) {
	r, _ := New(1, 8)
	r.Push(pkt(50))
	r.Push(pkt(51))
	res := r.Push(pkt(1)) // far behind
	if !res.TooOld {
		t.Fatalf("expected too-old: %+v", res)
	}
	if st := r.Stats(); st.TooOld != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestSetWindow(t *testing.T) {
	r, _ := New(1, 8)
	if err := r.SetWindow(4); err != nil || r.Window() != 4 {
		t.Fatalf("set window: %v", err)
	}
	if err := r.SetWindow(MaxWindow + 1); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("set invalid: %v", err)
	}
}

func TestGapsNeverInventMedia(t *testing.T) {
	r, _ := New(3, 2)
	r.Push(pkt(10))
	res := r.Push(pkt(20))
	if len(res.Gaps) != 1 || res.Gaps[0].Count != 9 {
		t.Fatalf("gaps %+v", res.Gaps)
	}
	if !eq(seqs(res.Ordered), []uint16{20}) {
		t.Fatalf("delivered %v", seqs(res.Ordered))
	}
}
