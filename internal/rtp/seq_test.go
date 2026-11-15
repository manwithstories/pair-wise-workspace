package rtp

import (
	"testing"

	pionrtp "github.com/pion/rtp"

	"rtspaudit/internal/fixture"
)

func packet(seq uint16) *pionrtp.Packet {
	return &pionrtp.Packet{Header: pionrtp.Header{Version: 2, SequenceNumber: seq}}
}

func TestSeqTrackerWrapReorderAndDuplicate(t *testing.T) {
	tr := NewSeqTrackerAt(65534, 32)
	var got []uint16
	for _, seq := range []uint16{65535, 65534, 0, 2, 1, 1} {
		out, dup, late := tr.Push(packet(seq))
		if late {
			t.Fatalf("unexpected late packet %d", seq)
		}
		_ = dup
		for _, op := range out {
			got = append(got, op.Packet.SequenceNumber)
		}
	}
	for _, op := range tr.Flush() {
		got = append(got, op.Packet.SequenceNumber)
	}
	want := []uint16{65534, 65535, 0, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if tr.Duplicates() != 1 {
		t.Fatalf("duplicates=%d", tr.Duplicates())
	}
}

func TestSeqTrackerGapAndLate(t *testing.T) {
	tr := NewSeqTrackerAt(100, 8)
	if _, _, late := tr.Push(packet(90)); !late || tr.LateDropped() != 1 {
		t.Fatalf("expected one late packet, late=%v count=%d", late, tr.LateDropped())
	}
	out, _, _ := tr.Push(packet(104))
	if len(out) != 0 {
		t.Fatalf("packet should still be buffered: %v", out)
	}
	out = tr.Flush()
	if len(out) != 1 || out[0].GapBefore != 4 || out[0].Packet.SequenceNumber != 104 {
		t.Fatalf("unexpected flush: %+v", out)
	}
	gaps := tr.Gaps()
	if len(gaps) != 1 || gaps[0].Start != 100 || gaps[0].End != 103 || gaps[0].Missing != 4 {
		t.Fatalf("unexpected gaps: %+v", gaps)
	}
}

func TestSeqTrackerWindowBound(t *testing.T) {
	tr := NewSeqTrackerAt(100, 4)
	for seq := uint16(101); seq < 110; seq++ {
		tr.Push(packet(seq))
	}
	if len(tr.pending) > 4 {
		t.Fatalf("pending=%d exceeds window", len(tr.pending))
	}
}

func TestSeqTrackerSingleMissingAtWindowEdge(t *testing.T) {
	tr := NewSeqTrackerAt(4099, 32)
	var got []uint16
	for seq := uint16(4100); seq <= 4131; seq++ {
		out, _, _ := tr.Push(packet(seq))
		for _, op := range out {
			got = append(got, op.Packet.SequenceNumber)
		}
	}
	if len(got) != 32 || got[0] != 4100 || got[31] != 4131 {
		t.Fatalf("got %v next=%d pending=%d gaps=%+v", got, tr.next, len(tr.pending), tr.gaps)
	}
}

func TestSeqTrackerFixtureImpairment(t *testing.T) {
	f, err := fixture.Generate(120)
	if err != nil {
		t.Fatal(err)
	}
	packets := fixture.Apply(f.Packetize(fixture.FirstSequence(7)), fixture.ImpairOptions{
		Loss: 0.02, Reorder: 0.01, Duplicate: 0.01, Seed: 7,
	})
	tr := NewSeqTrackerAt(packets[0].RTP.SequenceNumber, 32)
	for i := range packets {
		tr.Push(&packets[i].RTP)
	}
	tr.Flush()
	for _, gap := range tr.Gaps() {
		if gap.Missing > 32 {
			t.Fatalf("implausible gap %+v", gap)
		}
	}
}
