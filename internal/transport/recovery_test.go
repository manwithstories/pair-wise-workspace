package transport

import (
	"errors"
	"testing"

	"github.com/pion/rtp"
)

func group(seqs ...uint16) []*rtp.Packet {
	out := make([]*rtp.Packet, 0, len(seqs))
	for _, s := range seqs {
		out = append(out, pkt(s, byte(s*7)))
	}
	return out
}

func presentMap(pkts []*rtp.Packet) map[uint16]*rtp.Packet {
	m := map[uint16]*rtp.Packet{}
	for _, p := range pkts {
		m[p.SequenceNumber] = p
	}
	return m
}

func TestRecoverSinglePacket(t *testing.T) {
	full := group(10, 11, 12, 13)
	block, err := EncodeFEC(full)
	if err != nil {
		t.Fatal(err)
	}
	// Drop seq 11; everything else is present.
	present := presentMap([]*rtp.Packet{full[0], full[2], full[3]})
	recovered, lost, err := Recover(present, []uint16{11}, []FECBlock{block})
	if err != nil || len(lost) != 0 {
		t.Fatalf("single loss must recover: lost=%v err=%v", lost, err)
	}
	got, ok := recovered[11]
	if !ok {
		t.Fatal("recovered map missing seq 11")
	}
	want := full[1]
	if got.SSRC != want.SSRC || len(got.Payload) != len(want.Payload) {
		t.Fatalf("header/payload size mismatch: %+v", got)
	}
	for i := range want.Payload {
		if got.Payload[i] != want.Payload[i] {
			t.Fatalf("rebuilt payload byte %d = %d want %d", i, got.Payload[i], want.Payload[i])
		}
	}
}

func TestBurstLossUnrecoverable(t *testing.T) {
	full := group(20, 21, 22, 23)
	block, _ := EncodeFEC(full)
	present := presentMap([]*rtp.Packet{full[0], full[3]}) // 21 and 22 both gone
	recovered, lost, err := Recover(present, []uint16{21, 22}, []FECBlock{block})
	if err == nil {
		t.Fatal("burst loss must return error")
	}
	var le *LossError
	if !errors.As(err, &le) || !errors.Is(err, ErrUnrecoverable) {
		t.Fatalf("want LossError wrapping ErrUnrecoverable, got %T %v", err, err)
	}
	if len(lost) != 2 || lost[0] != 21 || lost[1] != 22 {
		t.Fatalf("lost positions = %v, want [21 22]", lost)
	}
	if len(recovered) != 0 {
		t.Fatalf("nothing should be fabricated, got %v", recovered)
	}
}

func TestRecoverAcrossTwoBlocks(t *testing.T) {
	g1 := group(0, 1, 2)
	g2 := group(3, 4, 5)
	b1, _ := EncodeFEC(g1)
	b2, _ := EncodeFEC(g2)
	present := presentMap([]*rtp.Packet{g1[0], g1[2], g2[0], g2[2]}) // missing 1 and 4
	rec, lost, err := Recover(present, []uint16{1, 4}, []FECBlock{b1, b2})
	if err != nil || len(lost) != 0 {
		t.Fatalf("independent single losses: lost=%v err=%v", lost, err)
	}
	if rec[1].Payload[0] != g1[1].Payload[0] || rec[4].Payload[0] != g2[1].Payload[0] {
		t.Fatalf("cross-block rebuild wrong: %x %x", rec[1].Payload, rec[4].Payload)
	}
}

func TestEncodeEmptyFEC(t *testing.T) {
	if _, err := EncodeFEC(nil); err == nil {
		t.Fatal("empty FEC group must error")
	}
}

func TestFECBlockCoversWrap(t *testing.T) {
	blk := FECBlock{FirstSeq: 65534, LastSeq: 1}
	if !blk.covers(65535) || !blk.covers(0) || blk.covers(2) || blk.covers(65533) {
		t.Fatal("wrapping block range membership wrong")
	}
}
