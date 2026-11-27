package transport

import (
	"errors"
	"testing"

	"github.com/pion/rtp"
)

func pkt(seq uint16, payload byte) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: seq, SSRC: 7, PayloadType: 96},
		Payload: []byte{payload, payload + 1, payload + 2},
	}
}

func TestJitterReordersAndDedupes(t *testing.T) {
	j := NewJitterBuffer(0)
	for _, seq := range []uint16{3, 1, 2, 1, 4} {
		if err := j.Push(pkt(seq, byte(seq))); err != nil {
			t.Fatal(err)
		}
	}
	got := j.Drain()
	want := []uint16{1, 2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("drained %d want %d", len(got), len(want))
	}
	for i, p := range got {
		if p.SequenceNumber != want[i] {
			t.Fatalf("pos %d = seq %d want %d", i, p.SequenceNumber, want[i])
		}
	}
	st := j.Stats()
	if st.Received != 5 || st.Duplicates != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestJitterHoleStallsAndGap(t *testing.T) {
	j := NewJitterBuffer(0)
	j.Push(pkt(1, 1))
	j.Push(pkt(3, 3)) // seq 2 missing
	if out := j.Drain(); len(out) != 1 || out[0].SequenceNumber != 1 {
		t.Fatalf("drain must stop at hole, got %+v", out)
	}
	gaps := j.Missing(nil)
	if len(gaps) != 1 || gaps[0].SeqNum != 2 {
		t.Fatalf("gaps = %+v", gaps)
	}
	j.Push(pkt(2, 2))
	out := j.Drain()
	if len(out) != 2 || out[0].SequenceNumber != 2 || out[1].SequenceNumber != 3 {
		t.Fatalf("post-fill drain = %+v", out)
	}
}

func TestJitterLateAndSkip(t *testing.T) {
	j := NewJitterBuffer(0)
	j.Push(pkt(1, 1))
	j.Drain() // base advances to 2, buffer empties
	// Re-establish a live base at 2 by buffering 3 (hole at 2).
	j.Push(pkt(3, 3))
	j.Push(pkt(1, 9)) // seq 1 is now behind the active base 2
	if st := j.Stats(); st.Late != 1 {
		t.Fatalf("late count = %d", st.Late)
	}
	// Skip the unrecoverable hole at 2 so packet 3 can flush.
	j.Skip(2)
	out := j.Drain()
	if len(out) != 1 || out[0].SequenceNumber != 3 {
		t.Fatalf("after skip = %+v", out)
	}
}

func TestJitterCapacityBound(t *testing.T) {
	j := NewJitterBuffer(4)
	// Fill the four distinct slots.
	for _, s := range []uint16{10, 11, 12, 13} {
		if err := j.Push(pkt(s, 1)); err != nil {
			t.Fatal(err)
		}
	}
	// A fifth distinct packet is refused while the buffer is saturated.
	if err := j.Push(pkt(14, 1)); !errors.Is(err, ErrBufferFull) {
		t.Fatalf("capacity bound: got %v want ErrBufferFull", err)
	}
	// Duplicates are still accepted-and-ignored, they need no new slot.
	if err := j.Push(pkt(11, 1)); err != nil {
		t.Fatalf("duplicate on full buffer must not error: %v", err)
	}
	// Draining one packet frees a slot for the next sequence number.
	j.Drain()
	if err := j.Push(pkt(14, 1)); err != nil {
		t.Fatalf("freed slot should accept: %v", err)
	}
}

func TestSeqDistanceWraparound(t *testing.T) {
	cases := []struct {
		a, b uint16
		want int32
	}{
		{65534, 65535, 1},
		{65535, 0, 1},
		{0, 65535, -1},
		{10, 12, 2},
	}
	for _, c := range cases {
		if got := seqDistance(c.a, c.b); got != c.want {
			t.Fatalf("dist(%d,%d)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}
