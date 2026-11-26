package transport

import "testing"

func pkt(track string, seq uint64) Packet {
	return Packet{
		ID:         PacketID{Stream: "event", Rendition: track, Sequence: seq},
		SourceNode: "edge-a",
		Data:       []byte{byte(seq), byte(seq >> 8)},
		Bytes:      2,
	}
}

func TestReorderDeduplicatesAndSorts(t *testing.T) {
	r, err := NewReorderer(4, 8, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res := r.Push(pkt("high", 1)); len(res.Ordered) != 1 {
		t.Fatalf("seq1 ordered=%d", len(res.Ordered))
	}
	r.Push(pkt("high", 3))
	res := r.Push(pkt("high", 2))
	if len(res.Ordered) != 2 || res.Ordered[0].ID.Sequence != 2 || res.Ordered[1].ID.Sequence != 3 {
		t.Fatalf("drain=%+v", res.Ordered)
	}
	dup := r.Push(pkt("high", 2))
	if !dup.Duplicate {
		t.Fatal("duplicate not detected")
	}
	st := r.Stats()
	if st.Duplicates != 1 || st.OutOfOrder != 1 {
		t.Fatalf("stats=%+v", st)
	}
}

func TestLatePacketAfterClosedGapIsNotSilentlyJoined(t *testing.T) {
	r, _ := NewReorderer(4, 8, 1)
	r.Push(pkt("low", 1))
	r.Push(pkt("low", 4))
	gaps := r.CloseGap(PacketID{Stream: "event", Rendition: "low", Sequence: 2}, 2)
	if gaps != nil {
		t.Fatalf("seq3 should still block: %+v", gaps)
	}
	gaps = r.CloseGap(PacketID{Stream: "event", Rendition: "low", Sequence: 3}, 2)
	if len(gaps) != 1 || gaps[0].ID.Sequence != 4 {
		t.Fatalf("closing seq3 should release seq4, got %+v", gaps)
	}
	late := r.Push(pkt("low", 2))
	if !late.Late || late.Fault == nil {
		t.Fatalf("late result=%+v", late)
	}
	if r.Stats().Late != 1 || r.Stats().Missing != 2 {
		t.Fatalf("stats=%+v", r.Stats())
	}
}

func TestOutsideFiniteWindowRejected(t *testing.T) {
	r, _ := NewReorderer(2, 4, 1)
	res := r.Push(pkt("mid", 3)) // frontier=1, width=2 allows only seq2 ahead
	if !res.Outside || res.Fault == nil {
		t.Fatalf("result=%+v want outside-window fault", res)
	}
}

func TestTracksAreIndependent(t *testing.T) {
	r, _ := NewReorderer(4, 8, 1)
	r.Push(pkt("high", 2))
	r.Push(pkt("low", 1))
	if got := r.OrderedSnapshot("event\x00high"); len(got) != 1 || got[0].ID.Sequence != 2 {
		t.Fatalf("high snapshot=%+v", got)
	}
}
