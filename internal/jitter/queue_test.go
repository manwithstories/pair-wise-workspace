package jitter

import (
	"testing"

	"mediad/internal/ingest"
	"mediad/internal/media"
)

func pkt(seq uint64, size int) *ingest.Packet {
	return &ingest.Packet{Track: 1, ExtSeq: seq, Payload: make([]byte, size)}
}

func tm(nanos int64) media.MediaTime {
	return media.MediaTime{Nanos: nanos, Rate: 48000}
}

func TestPopInExtendedSeqOrder(t *testing.T) {
	q := New(1, 0, 0)
	for _, s := range []uint64{5, 2, 9, 1, 7} {
		if f := q.Push(pkt(s, 10), tm(int64(s)*100)); f != nil {
			t.Fatal(f)
		}
	}
	want := []uint64{1, 2, 5, 7, 9}
	for _, w := range want {
		p, _, ok := q.Pop()
		if !ok || p.ExtSeq != w {
			t.Fatalf("pop order: got %v want %d", p, w)
		}
	}
	if q.Len() != 0 {
		t.Fatalf("queue not drained: %d", q.Len())
	}
}

func TestDurationBoundRecordsFirstDrop(t *testing.T) {
	q := New(1, 1000, 0) // 1 microsecond span allowed
	if f := q.Push(pkt(1, 10), tm(0)); f != nil {
		t.Fatal(f)
	}
	f := q.Push(pkt(2, 10), tm(5000))
	if f == nil || f.Kind != media.FaultBufferOverflow {
		t.Fatalf("want buffer-overflow fault, got %v", f)
	}
	if q.FirstDrop == nil || q.FirstDrop.ExtSeq != 2 || q.FirstDrop.Reason != "duration-limit" {
		t.Fatalf("first drop not recorded: %+v", q.FirstDrop)
	}
	// A later overflow must not overwrite the first record.
	q.Push(pkt(3, 10), tm(9000))
	if q.Drops != 2 || q.FirstDrop.ExtSeq != 2 {
		t.Fatalf("drops=%d first=%+v", q.Drops, q.FirstDrop)
	}
	if q.Len() != 1 {
		t.Fatalf("queue grew past bound: %d", q.Len())
	}
}

func TestByteBoundRecordsFirstDrop(t *testing.T) {
	q := New(1, 0, 100)
	if f := q.Push(pkt(1, 60), tm(0)); f != nil {
		t.Fatal(f)
	}
	f := q.Push(pkt(2, 60), tm(10))
	if f == nil || f.Kind != media.FaultBufferOverflow {
		t.Fatalf("want buffer-overflow fault, got %v", f)
	}
	if q.FirstDrop == nil || q.FirstDrop.Reason != "byte-limit" {
		t.Fatalf("first drop not recorded: %+v", q.FirstDrop)
	}
	// After popping, room is available again.
	if _, _, ok := q.Pop(); !ok {
		t.Fatal("pop failed")
	}
	if f := q.Push(pkt(3, 60), tm(20)); f != nil {
		t.Fatalf("push after pop should fit: %v", f)
	}
}

func TestDuplicateSeqIgnored(t *testing.T) {
	q := New(1, 0, 0)
	q.Push(pkt(4, 10), tm(0))
	if f := q.Push(pkt(4, 10), tm(0)); f != nil {
		t.Fatal(f)
	}
	if q.Len() != 1 {
		t.Fatalf("duplicate buffered: %d", q.Len())
	}
}
