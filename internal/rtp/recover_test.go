package rtp

import (
	"testing"

	pionrtp "github.com/pion/rtp"
)

func pkt(seq uint16) pionrtp.Packet {
	return pionrtp.Packet{Header: pionrtp.Header{Version: 2, SequenceNumber: seq}}
}

func feed(r *Recoverer, seqs ...uint16) {
	for _, s := range seqs {
		r.Feed(pkt(s))
	}
}

func TestInOrder(t *testing.T) {
	r := NewRecoverer(8)
	feed(r, 0, 1, 2, 3)
	r.Flush()
	s := r.Stats()
	if s.Received != 4 || s.Lost != 0 || s.Recovered != 0 || s.RecoveryPct != 100 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestReorderWithinWindow(t *testing.T) {
	r := NewRecoverer(8)
	feed(r, 0, 2, 1, 3)
	r.Flush()
	s := r.Stats()
	if s.Received != 3 || s.Recovered != 1 || s.Lost != 0 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestDuplicatesNeverDoubleCounted(t *testing.T) {
	r := NewRecoverer(8)
	feed(r, 0, 1, 1, 1, 2)
	r.Flush()
	s := r.Stats()
	if s.Received != 3 || s.Duplicates != 2 {
		t.Fatalf("retries must not be re-counted: %+v", s)
	}
}

func TestBurstLossRecoveryAndLate(t *testing.T) {
	r := NewRecoverer(4)
	// 5,6,7 dropped; 5,6 retransmitted inside the window; 7 never.
	// The window then expires 7 and the buffered run 8..12 is recovered.
	feed(r, 0, 1, 2, 3, 4, 8, 9, 10, 5, 6, 11, 12)
	// 7 arrives only after it was declared lost.
	feed(r, 7)
	r.Flush()
	s := r.Stats()
	if s.Recovered != 5 { // 8..12 recovered as a continuous run
		t.Fatalf("recovered: %+v", s)
	}
	if s.Lost != 1 || s.Late != 1 {
		t.Fatalf("lost/late: %+v", s)
	}
	want := 5.0 / 6.0 * 100
	if s.RecoveryPct < want-0.01 || s.RecoveryPct > want+0.01 {
		t.Fatalf("pct: %+v", s)
	}
}

func TestWindowCapExpiresGap(t *testing.T) {
	r := NewRecoverer(4)
	// Gap at 3 never fills; window must expire it, not grow without bound.
	feed(r, 0, 1, 2, 4, 5, 6, 7, 8, 9, 10)
	r.Flush()
	s := r.Stats()
	if s.Lost != 1 {
		t.Fatalf("gap must be expired exactly once: %+v", s)
	}
	if s.Received+s.Recovered != 10 {
		t.Fatalf("delivered count: %+v", s)
	}
}

func TestSequenceWrap(t *testing.T) {
	r := NewRecoverer(8)
	feed(r, 65534, 65535, 0, 1)
	r.Flush()
	s := r.Stats()
	if s.Received != 4 || s.Lost != 0 {
		t.Fatalf("wrap: %+v", s)
	}
}

func TestWindowClamped(t *testing.T) {
	if r := NewRecoverer(100000); r.window != MaxWindow {
		t.Fatalf("window not clamped: %d", r.window)
	}
	if r := NewRecoverer(0); r.window != 1 {
		t.Fatalf("window floor: %d", r.window)
	}
}
