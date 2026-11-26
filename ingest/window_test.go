package ingest

import (
	"errors"
	"testing"

	"medcheck/fault"
)

func testPacket(seq uint64) Packet {
	return Packet{Header: Header{Camera: "A", Stream: "video", Seq: seq}, Payload: []byte{byte(seq)}}
}

func TestPacketHeaderCRC(t *testing.T) {
	b := EncodePacket(testPacket(7))
	p, err := ParsePacket(b)
	if err != nil {
		t.Fatal(err)
	}
	if p.Seq != 7 || string(p.Payload) != "\x07" {
		t.Fatalf("bad packet parse: %+v payload=%v", p.Header, p.Payload)
	}
	b[10] ^= 1
	if _, err := ParsePacket(b); !errors.Is(err, ErrBadPacketCRC) {
		t.Fatalf("expected CRC error, got %v", err)
	}
}

func TestDedupReorderAndMonotonicRecovery(t *testing.T) {
	w := NewWindow(8, fault.NewModel())
	arrival := []uint64{0, 2, 1, 3, 3, 4, 6, 5}
	var got []uint64
	for _, seq := range arrival {
		ready, err := w.Ingest(testPacket(seq))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ready {
			got = append(got, p.Seq)
		}
	}
	// Duplicate 3 is dropped; all other sequences become monotonic.
	want := []uint64{0, 1, 2, 3, 4, 5, 6}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order=%v want %v", got, want)
		}
	}
	st := w.Stats()
	if st.Duplicates != 1 || st.Reordered != 2 {
		t.Fatalf("unexpected stats: %+v", st)
	}
}

func TestFullWindowAppliesBackpressureUntilFrontierAdvances(t *testing.T) {
	fm := fault.NewModel()
	w := NewWindow(2, fm)
	// Buffer seq 1 and 2 while seq 0 is missing.
	for _, seq := range []uint64{1, 2} {
		if _, err := w.Ingest(testPacket(seq)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Ingest(testPacket(3)); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("expected backpressure, got %v", err)
	}
	ready, err := w.Ingest(testPacket(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 3 || ready[0].Seq != 0 || ready[2].Seq != 2 {
		t.Fatalf("frontier flush wrong: %+v", ready)
	}
	if _, err := w.Ingest(testPacket(3)); err != nil {
		t.Fatalf("packet should fit after drain: %v", err)
	}
}

func TestBurstLossSpan(t *testing.T) {
	fm := fault.NewModel()
	w := NewWindow(16, fm)
	for _, seq := range []uint64{0, 1, 6, 7, 8, 9} {
		if _, err := w.Ingest(testPacket(seq)); err != nil {
			t.Fatal(err)
		}
	}
	src := Source{Camera: "A", Stream: "video"}
	spans := w.RecordLosses(src, 9)
	if len(spans) != 1 || spans[0].StartSeq != 2 || spans[0].EndSeq != 5 {
		t.Fatalf("expected precise 4-packet burst, got %+v", spans)
	}
	if w.Stats().BurstLost != 4 {
		t.Fatalf("burst_lost=%d want 4", w.Stats().BurstLost)
	}
}
