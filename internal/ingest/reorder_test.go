package ingest

import (
	"testing"

	"github.com/pion/rtp"
)

func seqRecord(seq uint16) *Record {
	return &Record{Packet: &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}}}
}

func extSeqs(ps []OrderedPacket) []int64 {
	out := make([]int64, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ExtSeq)
	}
	return out
}

func gapSeqs(gs []Gap) []int64 {
	out := make([]int64, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.ExtSeq)
	}
	return out
}

func TestReorderInOrderAndDuplicate(t *testing.T) {
	b := NewReorderBuffer(8)
	for _, seq := range []uint16{10, 11, 12} {
		res := b.Feed(seqRecord(seq))
		if len(res.Ordered) != 1 || res.Ordered[0].Record.Packet.Header.SequenceNumber != seq {
			t.Fatalf("seq %d result=%+v", seq, res)
		}
	}
	res := b.Feed(seqRecord(11))
	if !res.Duplicate || b.Stats.DuplicatePackets != 1 {
		t.Fatalf("duplicate result=%+v stats=%+v", res, b.Stats)
	}
}

func TestReorderWithinBoundedWindow(t *testing.T) {
	b := NewReorderBuffer(4)
	if got := extSeqs(b.Feed(seqRecord(0)).Ordered); len(got) != 1 || got[0] != 0 {
		t.Fatalf("start=%v", got)
	}
	res2 := b.Feed(seqRecord(2))
	if len(res2.Ordered) != 0 || len(res2.Gaps) != 0 {
		t.Fatalf("seq2 should wait: %+v", res2)
	}
	res1 := b.Feed(seqRecord(1))
	if got := extSeqs(res1.Ordered); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("seq1 release = %v", got)
	}
	if b.Stats.OutOfOrderPackets != 1 {
		t.Fatalf("out-of-order count=%d", b.Stats.OutOfOrderPackets)
	}
}

func TestGapThenLatePacket(t *testing.T) {
	b := NewReorderBuffer(2)
	b.Feed(seqRecord(0))
	res := b.Feed(seqRecord(3))
	if got := gapSeqs(res.Gaps); len(got) != 1 || got[0] != 1 {
		t.Fatalf("gaps=%v", got)
	}
	res = b.Feed(seqRecord(2))
	if got := extSeqs(res.Ordered); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("ordered=%v", got)
	}
	late := b.Feed(seqRecord(1))
	if len(late.Late) != 1 || late.Late[0].ExtSeq != 1 {
		t.Fatalf("late=%+v", late.Late)
	}
	if b.Stats.GapPackets != 1 || b.Stats.LatePackets != 1 {
		t.Fatalf("stats=%+v", b.Stats)
	}
}

func TestSequenceWraparound(t *testing.T) {
	b := NewReorderBuffer(4)
	want := []int64{65534, 65535, 65536, 65537}
	seqs := []uint16{65534, 65535, 0, 1}
	for i, seq := range seqs {
		res := b.Feed(seqRecord(seq))
		if len(res.Ordered) != 1 || res.Ordered[0].ExtSeq != want[i] {
			t.Fatalf("feed %d got=%+v want %d", seq, res.Ordered, want[i])
		}
	}
}

func TestLargeJumpIsBoundedAndFlushCompletes(t *testing.T) {
	b := NewReorderBuffer(2)
	b.Feed(seqRecord(0))
	res := b.Feed(seqRecord(100))
	if len(res.Gaps) != 98 {
		t.Fatalf("declared gaps=%d, want 98", len(res.Gaps))
	}
	if b.Pending() > b.Window {
		t.Fatalf("pending=%d exceeds window", b.Pending())
	}
	res = b.Flush()
	if len(res.Ordered) != 1 || res.Ordered[0].ExtSeq != 100 {
		t.Fatalf("flush ordered=%+v", res.Ordered)
	}
}
