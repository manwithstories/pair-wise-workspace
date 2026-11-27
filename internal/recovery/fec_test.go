package recovery

import (
	"reflect"
	"testing"

	"avrepair/internal/segment"
)

func tsPacket(fill byte) []byte {
	p := make([]byte, segment.TSPacketLen)
	p[0] = 0x47
	for i := 1; i < len(p); i++ {
		p[i] = fill
	}
	return p
}

func group8() [][]byte {
	out := make([][]byte, GroupSize)
	for i := range out {
		out[i] = tsPacket(byte(i + 1))
	}
	return out
}

func TestRecoverSingleLoss(t *testing.T) {
	group := group8()
	par := BuildParity(0, group)
	var pkts []segment.Packet
	for i := 0; i < GroupSize; i++ {
		if i == 3 {
			continue // lost
		}
		pkts = append(pkts, segment.Packet{Seq: uint16(i), Payload: group[i]})
	}
	pkts = append(pkts, segment.Packet{Seq: 8, Parity: true, Base: 0, Payload: par})
	res := Recover(pkts)
	if res.Recovered != 1 || res.Missing != 1 || len(res.Packets) != GroupSize {
		t.Fatalf("result: %+v", res)
	}
	if !reflect.DeepEqual(res.Packets[3].Payload, group[3]) {
		t.Fatal("recovered payload mismatch")
	}
}

func TestRecoverOrderingAndDuplicates(t *testing.T) {
	group := group8()
	pkts := []segment.Packet{
		{Seq: 2, Payload: group[2]},
		{Seq: 0, Payload: group[0]},
		{Seq: 2, Payload: group[2]}, // duplicate
		{Seq: 1, Payload: group[1]},
		{Seq: 1, Payload: group[1]}, // duplicate
	}
	res := Recover(pkts)
	if res.Duplicates != 2 || res.Reordered < 1 {
		t.Fatalf("counters: %+v", res)
	}
	for i := 0; i < 3; i++ {
		if res.Packets[i].Seq != uint16(i) {
			t.Fatalf("order: %+v", res.Packets)
		}
	}
}

func TestRecoverDoubleLossIsUnrecoverable(t *testing.T) {
	group := group8()
	par := BuildParity(8, group)
	var pkts []segment.Packet
	for i := 0; i < GroupSize; i++ {
		if i == 1 || i == 5 {
			continue
		}
		pkts = append(pkts, segment.Packet{Seq: uint16(8 + i), Payload: group[i]})
	}
	pkts = append(pkts, segment.Packet{Seq: 16, Parity: true, Base: 8, Payload: par})
	res := Recover(pkts)
	if res.Recovered != 0 || len(res.Unrecoverable) != 1 {
		t.Fatalf("result: %+v", res)
	}
	if len(res.Unrecoverable[0].Missing) != 2 || len(res.Packets) != 6 {
		t.Fatalf("loss/missing: %+v", res)
	}
}

func TestRecoverContinuesAfterUnrecoverableGroup(t *testing.T) {
	g1 := group8()
	g2 := group8()
	p1 := BuildParity(0, g1)
	p2 := BuildParity(8, g2)
	pkts := []segment.Packet{{Seq: 8, Parity: true, Base: 0, Payload: p1}}
	for i := 0; i < GroupSize; i++ {
		if i == 0 || i == 2 {
			continue // two losses in group 0
		}
		pkts = append(pkts, segment.Packet{Seq: uint16(i), Payload: g1[i]})
	}
	for i := 0; i < GroupSize; i++ {
		if i == 4 {
			continue // one loss in group 8, should be repaired
		}
		pkts = append(pkts, segment.Packet{Seq: uint16(8 + i), Payload: g2[i]})
	}
	pkts = append(pkts, segment.Packet{Seq: 9, Parity: true, Base: 8, Payload: p2})
	res := Recover(pkts)
	if res.Recovered != 1 || len(res.Unrecoverable) != 1 {
		t.Fatalf("result: %+v", res)
	}
	for _, p := range res.Packets {
		if p.Seq == 12 && !reflect.DeepEqual(p.Payload, g2[4]) {
			t.Fatal("second group should still be repaired")
		}
	}
}
