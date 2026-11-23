package timeline

import (
	"testing"

	"mediarchive/internal/clock"
	"mediarchive/internal/event"
)

func pkt(seq uint64, mt clock.Time) *event.Packet {
	return &event.Packet{
		SeqExt: seq, TSExt: seq * 960,
		MediaTime: mt, Duration: 1800,
		Payload: []byte{byte(seq), byte(seq >> 8)},
	}
}

func TestWatermarkVerdicts(t *testing.T) {
	w := NewWatermark(1000)
	if v := w.Observe(5000); v != OnTime {
		t.Fatalf("first: %v", v)
	}
	if v := w.Observe(6000); v != OnTime {
		t.Fatalf("advance: %v", v)
	}
	if v := w.Observe(5500); v != Late { // 500 behind, window 1000
		t.Fatalf("late: %v", v)
	}
	if v := w.Observe(4000); v != TooLate { // 2000 behind
		t.Fatalf("too late: %v", v)
	}
	cur, ok := w.Current()
	if !ok || cur != 6000 {
		t.Fatalf("current=%d ok=%v", cur, ok)
	}
}

// Replay after a late arrival must produce exactly the summary that
// one-pass in-order processing produces.
func TestReplayMatchesInOrder(t *testing.T) {
	base := clock.Time(1800 * 4)
	inOrder := New(10000)
	for i := uint64(0); i < 8; i++ {
		inOrder.Add(pkt(i, base+clock.Time(i)*1800))
	}
	// Same packets, but packet 3 arrives after 4 and 5 (late, in window).
	scrambled := New(10000)
	order := []uint64{0, 1, 2, 4, 5, 3, 6, 7}
	for _, i := range order {
		scrambled.Add(pkt(i, base+clock.Time(i)*1800))
	}
	if scrambled.Replays != 1 {
		t.Fatalf("replays=%d want 1", scrambled.Replays)
	}
	if inOrder.Digest() != scrambled.Digest() {
		t.Fatal("replay digest differs from in-order digest")
	}
}

func TestTooLateExcludedAndMarked(t *testing.T) {
	tl := New(100) // window smaller than one packet
	tl.Add(pkt(0, 0))
	tl.Add(pkt(1, 1800))
	tl.Add(pkt(2, 3600))
	// Packet with media time 900 arrives when the watermark is at 3600.
	v := tl.Add(pkt(9, 900))
	if v != TooLate {
		t.Fatalf("verdict=%v want TooLate", v)
	}
	if tl.TooLate != 1 || len(tl.Unrepairable) != 1 {
		t.Fatalf("tooLate=%d marks=%d", tl.TooLate, len(tl.Unrepairable))
	}
	if tl.Unrepairable[0].Reason != event.ReasonTooLate {
		t.Fatalf("reason=%s", tl.Unrepairable[0].Reason)
	}
	if n := len(tl.Sorted()); n != 3 {
		t.Fatalf("sorted=%d want 3 (too-late excluded)", n)
	}
}

func TestDigestDeterministicAcrossDuplicates(t *testing.T) {
	a := New(10000)
	b := New(10000)
	for i := uint64(0); i < 5; i++ {
		a.Add(pkt(i, clock.Time(i)*1800))
		b.Add(pkt(i, clock.Time(i)*1800))
	}
	// A duplicate inserted into the log must not change the canonical form.
	b.Add(pkt(2, 3600))
	if a.Digest() != b.Digest() {
		t.Fatal("duplicate changed canonical digest")
	}
	if n := len(b.Sorted()); n != 5 {
		t.Fatalf("sorted=%d want 5", n)
	}
}
