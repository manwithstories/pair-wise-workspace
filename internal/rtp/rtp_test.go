package rtp

import (
	"testing"
)

func makeStream(n int, start uint16) []Packet {
	out := make([]Packet, n)
	for i := 0; i < n; i++ {
		out[i] = Packet{Seq: start + uint16(i), TS: uint32(i) * 3000, Frame: i, Keyframe: i%10 == 0}
	}
	return out
}

func TestPacketRoundTrip(t *testing.T) {
	p := Packet{Seq: 42, TS: 900, Frame: 7, Keyframe: true}
	if err := p.Encode(1234); err != nil {
		t.Fatal(err)
	}
	got, err := Decode(p.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != p.Seq || got.TS != p.TS || got.Frame != p.Frame || !got.Keyframe {
		t.Fatalf("round trip=%+v", got)
	}
	if _, err := Decode([]byte{1, 2, 3}); err == nil {
		t.Fatal("short packet accepted")
	}
}

func TestReordererDuplicatesAndSwaps(t *testing.T) {
	r, err := NewReorderer(ReorderOptions{DuplicateEvery: 3, SwapEvery: 4, StartSeq: 0})
	if err != nil {
		t.Fatal(err)
	}
	in := makeStream(6, 100)
	out := r.Apply(in)
	if len(out) != 8 { // 6 + 2 duplicates
		t.Fatalf("len=%d", len(out))
	}
	// The first swap happens around index 3, so sequence 103 arrives after 104.
	found := false
	for i := 0; i+1 < len(out); i++ {
		if out[i].Seq > out[i+1].Seq {
			found = true
		}
	}
	if !found {
		t.Fatal("no reordering produced")
	}
	if _, err := NewReorderer(ReorderOptions{DuplicateEvery: 1}); err == nil {
		t.Fatal("period 1 accepted")
	}
	if _, err := NewReorderer(ReorderOptions{SwapEvery: -2}); err == nil {
		t.Fatal("negative period accepted")
	}
}

func TestReceiverRestoresOrder(t *testing.T) {
	r, _ := NewReorderer(ReorderOptions{SwapEvery: 2})
	stream := makeStream(20, 0)
	arrivals := r.Apply(stream)
	out, stats := DeliverAll(arrivals, 8)
	if len(out) != len(stream) {
		t.Fatalf("delivered=%d want %d", len(out), len(stream))
	}
	for i, p := range out {
		if p.Frame != i {
			t.Fatalf("frame %d out of order: %d", i, p.Frame)
		}
	}
	if stats.Lost != 0 || stats.Duplicate != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	if stats.Reordered == 0 {
		t.Fatal("expected reordered packets")
	}
}

func TestReceiverCountsDuplicates(t *testing.T) {
	r, _ := NewReorderer(ReorderOptions{DuplicateEvery: 2})
	out, stats := DeliverAll(r.Apply(makeStream(10, 0)), 8)
	if len(out) != 10 {
		t.Fatalf("delivered=%d", len(out))
	}
	if stats.Duplicate != 5 {
		t.Fatalf("duplicates=%d", stats.Duplicate)
	}
}

func TestReceiverHandlesSequenceWraparound(t *testing.T) {
	// Start near the uint16 wrap so the sequence crosses 65535 -> 0.
	stream := makeStream(30, 65520)
	r, _ := NewReorderer(ReorderOptions{SwapEvery: 3})
	out, stats := DeliverAll(r.Apply(stream), 8)
	if len(out) != len(stream) {
		t.Fatalf("delivered=%d want %d (stats %+v)", len(out), len(stream), stats)
	}
	for i, p := range out {
		if p.Frame != i {
			t.Fatalf("frame %d out of order: %d", i, p.Frame)
		}
	}
}

func TestReceiverBoundedWindowDropsLatePackets(t *testing.T) {
	// A packet far behind next must be discarded, not buffered.
	recv := NewReceiver(10, 4)
	recv.Push(Packet{Seq: 10, Frame: 0})
	recv.Push(Packet{Seq: 11, Frame: 1})
	recv.next = 15                       // simulate the window having advanced
	recv.Push(Packet{Seq: 12, Frame: 2}) // late
	st := recv.Stats()
	if st.Late+st.Duplicate == 0 {
		t.Fatalf("late packet not counted: %+v", st)
	}
	if recv.Buffered() > 4 {
		t.Fatalf("buffer unbounded: %d", recv.Buffered())
	}
}

func TestReceiverExpireCountsLostGap(t *testing.T) {
	recv := NewReceiver(0, 4)
	recv.Push(Packet{Seq: 0, Frame: 0})
	recv.Push(Packet{Seq: 3, Frame: 3}) // 1,2 never arrive
	recv.Expire()
	st := recv.Stats()
	if st.Lost != 2 {
		t.Fatalf("lost=%d want 2 (stats %+v)", st.Lost, st)
	}
}
