package transport

import (
	"testing"

	"github.com/pion/rtp"
)

func pkt(seq uint16, payload ...byte) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, SSRC: 0x42},
		Payload: payload,
	}
}

func seqs(res Result) []uint16 {
	var out []uint16
	for _, p := range res.Packets {
		out = append(out, p.SequenceNumber)
	}
	return out
}

func TestWraparoundUnfolded(t *testing.T) {
	res := Process([]*rtp.Packet{
		pkt(65534, 1), pkt(65535, 2), pkt(0, 3), pkt(1, 4),
	})
	got := seqs(res)
	want := []uint16{65534, 65535, 0, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order=%v want %v", got, want)
		}
	}
	if res.Stats.Lost != 0 || res.Stats.Duplicates != 0 {
		t.Fatalf("stats=%+v", res.Stats)
	}
	if res.Stats.UniqueBytes != 4 {
		t.Fatalf("unique bytes=%d", res.Stats.UniqueBytes)
	}
}

func TestDuplicatesNotCountedInBitrate(t *testing.T) {
	res := Process([]*rtp.Packet{
		pkt(10, 1, 2, 3), pkt(10, 1, 2, 3), pkt(11, 4), pkt(10, 1, 2, 3),
	})
	if res.Stats.Received != 4 || res.Stats.Duplicates != 2 {
		t.Fatalf("stats=%+v", res.Stats)
	}
	if res.Stats.UniqueBytes != 4 { // 3 + 1, duplicates excluded
		t.Fatalf("unique bytes=%d", res.Stats.UniqueBytes)
	}
	if len(res.Packets) != 2 {
		t.Fatalf("packets=%d", len(res.Packets))
	}
}

func TestOutOfOrderReassembled(t *testing.T) {
	res := Process([]*rtp.Packet{
		pkt(3, 4), pkt(1, 2), pkt(2, 3), pkt(0, 1),
	})
	got := seqs(res)
	want := []uint16{0, 1, 2, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order=%v want %v", got, want)
		}
	}
	if res.Stats.Lost != 0 {
		t.Fatalf("lost=%d", res.Stats.Lost)
	}
}

func TestLossReportedAsRange(t *testing.T) {
	res := Process([]*rtp.Packet{
		pkt(0, 1), pkt(1, 2), pkt(5, 6), pkt(6, 7),
	})
	if res.Stats.Lost != 3 {
		t.Fatalf("lost=%d", res.Stats.Lost)
	}
	if len(res.Stats.LostRanges) != 1 || res.Stats.LostRanges[0] != [2]uint64{2, 4} {
		t.Fatalf("ranges=%v", res.Stats.LostRanges)
	}
}

func TestWraparoundWithLossAndDuplicates(t *testing.T) {
	res := Process([]*rtp.Packet{
		pkt(65534, 1), pkt(65535, 2), pkt(0, 3), pkt(0, 3), pkt(2, 5),
	})
	if res.Stats.Duplicates != 1 {
		t.Fatalf("duplicates=%d", res.Stats.Duplicates)
	}
	if res.Stats.Lost != 1 {
		t.Fatalf("lost=%d", res.Stats.Lost)
	}
	if len(res.Stats.LostRanges) != 1 || res.Stats.LostRanges[0] != [2]uint64{65537, 65537} {
		t.Fatalf("ranges=%v", res.Stats.LostRanges)
	}
	if res.Stats.UniqueBytes != 4 {
		t.Fatalf("unique bytes=%d", res.Stats.UniqueBytes)
	}
}
