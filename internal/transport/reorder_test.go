package transport

import (
	"testing"

	"archivecheck/internal/fault"
)

func pkts(seqs ...uint16) []Packet {
	out := make([]Packet, len(seqs))
	for i, s := range seqs {
		out[i] = Packet{Seq: s, Arrival: i, Payload: []byte{byte(i)}}
	}
	return out
}

func TestInOrderNoGaps(t *testing.T) {
	fs := &fault.Set{}
	res := NewReorderer(2).Process(pkts(1, 2, 3, 4, 5), fs)
	if len(res.Packets) != 5 || res.Duplicates != 0 || res.Reordered != 0 || len(res.Gaps) != 0 {
		t.Fatalf("res=%+v", res)
	}
	if fs.Len() != 0 {
		t.Fatalf("faults=%v", fs.Faults())
	}
	if res.FirstSeq != 1 || res.LastSeq != 5 || res.Lost != 0 {
		t.Fatalf("extent %d..%d lost=%d", res.FirstSeq, res.LastSeq, res.Lost)
	}
}

func TestBurstLossBecomesExplicitGap(t *testing.T) {
	fs := &fault.Set{}
	// 5,6,7 arrive; 8,9,10 lost; 11.. resume.
	res := NewReorderer(2).Process(pkts(5, 6, 7, 11, 12), fs)
	if len(res.Gaps) != 1 {
		t.Fatalf("gaps=%+v", res.Gaps)
	}
	g := res.Gaps[0]
	if g.Start != 8 || g.End != 10 || g.Len() != 3 {
		t.Fatalf("gap=%+v", g)
	}
	if res.Lost != 3 || fs.Count(fault.KindPacketGap) != 1 {
		t.Fatalf("lost=%d faults=%v", res.Lost, fs.Faults())
	}
	// No fabricated frames: only the 5 survivors remain.
	if len(res.Packets) != 5 {
		t.Fatalf("packets=%d", len(res.Packets))
	}
	if got := res.Reassemble(); len(got) != 5 {
		t.Fatalf("reassembled %d bytes", len(got))
	}
	if res.LossRate() < 0.374 || res.LossRate() > 0.376 {
		t.Fatalf("loss rate %v", res.LossRate())
	}
}

func TestDuplicatesDropped(t *testing.T) {
	fs := &fault.Set{}
	res := NewReorderer(2).Process(pkts(1, 2, 2, 3, 3, 4), fs)
	if res.Duplicates != 2 {
		t.Fatalf("duplicates=%d", res.Duplicates)
	}
	if len(res.Packets) != 4 {
		t.Fatalf("packets=%d want 4", len(res.Packets))
	}
	if fs.Count(fault.KindPacketDuplicate) != 2 {
		t.Fatalf("faults=%v", fs.Faults())
	}
}

func TestReorderWithinTolerance(t *testing.T) {
	fs := &fault.Set{}
	res := NewReorderer(3).Process(pkts(1, 2, 4, 3, 5, 7, 6, 8), fs)
	if len(res.Packets) != 8 {
		t.Fatalf("packets=%d", len(res.Packets))
	}
	for i, p := range res.Packets {
		if p.Seq != uint16(i+1) {
			t.Fatalf("position %d seq %d", i, p.Seq)
		}
	}
	if res.Reordered != 2 {
		t.Fatalf("reordered=%d", res.Reordered)
	}
	if len(res.Gaps) != 0 {
		t.Fatalf("no gap should be inferred: %+v", res.Gaps)
	}
	if fs.Count(fault.KindPacketReorder) != 1 {
		t.Fatalf("faults=%v", fs.Faults())
	}
}

func TestSequenceWrapAround(t *testing.T) {
	res := NewReorderer(2).Process(pkts(65533, 65534, 65535, 0, 1, 2), &fault.Set{})
	if len(res.Packets) != 6 {
		t.Fatalf("packets=%d", len(res.Packets))
	}
	if len(res.Gaps) != 0 {
		t.Fatalf("wrap should not create gaps: %+v", res.Gaps)
	}
	// Extended numbers must still be strictly increasing.
	for i := 1; i < len(res.Packets); i++ {
		if res.Packets[i].ExtSeq <= res.Packets[i-1].ExtSeq {
			t.Fatalf("non-monotonic at %d", i)
		}
	}
}

func TestReorderAcrossWrap(t *testing.T) {
	// Wrap boundary with a swap: ...65534, 0, 65535, 1
	res := NewReorderer(2).Process(pkts(65533, 65534, 0, 65535, 1), &fault.Set{})
	if len(res.Packets) != 5 {
		t.Fatalf("packets=%d", len(res.Packets))
	}
	got := []uint16{65533, 65534, 65535, 0, 1}
	for i, want := range got {
		if res.Packets[i].Seq != want {
			t.Fatalf("pos %d seq %d want %d", i, res.Packets[i].Seq, want)
		}
	}
}

func TestEmptyAndSingle(t *testing.T) {
	fs := &fault.Set{}
	if res := NewReorderer(0).Process(nil, fs); len(res.Packets) != 0 || res.LossRate() != 0 {
		t.Fatalf("empty: %+v", res)
	}
	res := NewReorderer(0).Process(pkts(7), fs)
	if len(res.Packets) != 1 || res.FirstSeq != 7 {
		t.Fatalf("single: %+v", res)
	}
}

func TestRTPRoundTrip(t *testing.T) {
	orig := []Packet{
		{Seq: 10, Payload: []byte("hello"), Marker: true, Timestamp: 9000, SSRC: 0xDEADBEEF},
		{Seq: 11, Payload: []byte("world"), Timestamp: 9000, SSRC: 0xDEADBEEF},
	}
	var wire [][]byte
	for _, p := range orig {
		b, err := p.ToRTP()
		if err != nil {
			t.Fatal(err)
		}
		wire = append(wire, b)
	}
	back, err := PacketsFromRTP(wire)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[0].Seq != 10 || string(back[1].Payload) != "world" || !back[0].Marker {
		t.Fatalf("round trip: %+v", back)
	}
}
