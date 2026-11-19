package reorder

import (
	"testing"

	"mediapipeline/internal/mediaerr"
	"mediapipeline/internal/stream"
)

func mkPkt(ext uint64, rawSize int) *stream.Packet {
	raw := make([]byte, rawSize)
	return &stream.Packet{TrackID: 7, SSRC: 7, Sequence: uint16(ext), ExtSeq: ext, Raw: raw}
}

func TestInOrderAndReorder(t *testing.T) {
	b, merr := New(7, Config{WindowSize: 8, MaxBytes: 1 << 20})
	if merr != nil {
		t.Fatal(merr)
	}
	order := []uint64{0, 1, 3, 2, 4}
	for _, s := range order {
		if _, e := b.Push(mkPkt(s, 16)); e != nil {
			t.Fatalf("push %d: %v", s, e)
		}
	}
	out := b.Drain()
	got := make([]uint64, len(out))
	for i, p := range out {
		got[i] = p.ExtSeq
	}
	want := []uint64{0, 1, 2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("delivered %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivered %v want %v", got, want)
		}
	}
	if st := b.Stats(); st.Delivered != 5 || st.Gaps != 0 {
		t.Fatalf("stats wrong: %+v", st)
	}
}

func TestLatePacket(t *testing.T) {
	b, _ := New(7, Config{WindowSize: 4, MaxBytes: 1 << 20})
	for _, s := range []uint64{0, 1, 2} {
		if _, e := b.Push(mkPkt(s, 16)); e != nil {
			t.Fatal(e)
		}
	}
	b.Drain() // frontier moves to 3
	if _, e := b.Push(mkPkt(1, 16)); e == nil || e.Kind != mediaerr.KindLate {
		t.Fatalf("expected late error, got %v", e)
	}
	if b.Stats().Late != 1 {
		t.Fatalf("late count wrong: %+v", b.Stats())
	}
}

func TestWindowGapDeclaration(t *testing.T) {
	b, _ := New(7, Config{WindowSize: 4, MaxBytes: 1 << 20})
	b.Push(mkPkt(0, 16))
	// seq 8 forces expected to 8-4+1=5, declaring gaps 1..4.
	gaps, e := b.Push(mkPkt(8, 16))
	if e != nil {
		t.Fatal(e)
	}
	if len(gaps) != 1 || gaps[0].From != 1 || gaps[0].To != 4 {
		t.Fatalf("gaps wrong: %+v", gaps)
	}
	// seq 2 is now permanently late.
	if _, e := b.Push(mkPkt(2, 16)); e == nil || e.Kind != mediaerr.KindLate {
		t.Fatalf("expected late after window move, got %v", e)
	}
	if st := b.Stats(); st.Gaps != 4 || st.Late != 1 {
		t.Fatalf("stats wrong: %+v", st)
	}
}

func TestDuplicateIsolation(t *testing.T) {
	b, _ := New(7, Config{WindowSize: 8, MaxBytes: 1 << 20})
	b.Push(mkPkt(0, 16))
	_, e := b.Push(mkPkt(0, 16)) // same ext seq, isolated regardless of content
	if e == nil || e.Kind != mediaerr.KindDuplicate {
		t.Fatalf("expected duplicate error, got %v", e)
	}
	if b.Stats().Isolated != 1 || b.Len() != 1 {
		t.Fatalf("duplicate must not be stored: %+v len=%d", b.Stats(), b.Len())
	}
}

func TestByteBudget(t *testing.T) {
	b, _ := New(7, Config{WindowSize: 64, MaxBytes: 48})
	if _, e := b.Push(mkPkt(0, 20)); e != nil {
		t.Fatal(e)
	}
	if _, e := b.Push(mkPkt(1, 20)); e != nil {
		t.Fatal(e)
	}
	// 40 bytes held; the next push would reach 60 > 48.
	_, e := b.Push(mkPkt(2, 20))
	if e == nil || e.Kind != mediaerr.KindBufferLimit {
		t.Fatalf("expected buffer limit, got %v", e)
	}
	if b.Stats().OverLimit != 1 || b.Bytes() != 40 {
		t.Fatalf("budget stats wrong: %+v bytes=%d", b.Stats(), b.Bytes())
	}
	if b.Stats().PeakBytes != 40 {
		t.Fatalf("peak bytes wrong: %d", b.Stats().PeakBytes)
	}
}

func TestFlushGapsAndRemainder(t *testing.T) {
	b, _ := New(7, Config{WindowSize: 16, MaxBytes: 1 << 20})
	b.Push(mkPkt(0, 16))
	b.Push(mkPkt(2, 16)) // missing 1
	b.Drain()            // delivers 0, frontier at 1
	pkts, gaps := b.Flush()
	if len(pkts) != 1 || pkts[0].ExtSeq != 2 {
		t.Fatalf("flush packets wrong: %+v", pkts)
	}
	if len(gaps) != 1 || gaps[0].From != 1 || gaps[0].To != 1 {
		t.Fatalf("flush gaps wrong: %+v", gaps)
	}
	if b.Len() != 0 {
		t.Fatal("flush must empty the buffer")
	}
}

func TestInvalidConfig(t *testing.T) {
	if _, e := New(7, Config{WindowSize: 0, MaxBytes: 10}); e == nil {
		t.Fatal("expected error for zero window")
	}
	if _, e := New(7, Config{WindowSize: 4, MaxBytes: 0}); e == nil {
		t.Fatal("expected error for zero byte budget")
	}
}
