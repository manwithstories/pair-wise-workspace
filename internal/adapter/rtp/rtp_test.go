package rtp

import (
	"bytes"
	"testing"

	"mediacheck/internal/model"
)

func TestPacketizeDepacketizeRoundTrip(t *testing.T) {
	payload := make([]byte, maxPayload*2+300)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	pz := NewPacketizer(0x4242)
	pkts, err := pz.Packetize(7, payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) != 3 {
		t.Fatalf("packets = %d, want 3", len(pkts))
	}
	if !pkts[len(pkts)-1].Marker {
		t.Fatal("last packet must set marker")
	}
	if pkts[0].CSRC[0] != 7 {
		t.Fatalf("fragment seq in CSRC = %d", pkts[0].CSRC[0])
	}
	seq, got, err := Depacketize(pkts)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 7 || !bytes.Equal(got, payload) {
		t.Fatal("reassembled payload mismatch")
	}
}

func TestMarshalUnmarshalBatch(t *testing.T) {
	pz := NewPacketizer(1)
	pkts, _ := pz.Packetize(1, make([]byte, 3000))
	raw, err := MarshalBatch(pkts)
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalBatch(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(pkts) || back[0].SSRC != 1 {
		t.Fatal("batch unmarshal mismatch")
	}
}

func TestDepacketizeRejectsGapAndBadVersion(t *testing.T) {
	pz := NewPacketizer(9)
	pkts, _ := pz.Packetize(1, make([]byte, maxPayload*2))
	// 制造序号缺口。
	pkts[1].SequenceNumber++
	_, _, err := Depacketize(pkts)
	me, ok := model.AsMediaError(err)
	if !ok || me.Category != model.ErrFragmentGap {
		t.Fatalf("want fragment_gap, got %v", err)
	}
	// 版本错误。
	pkts2, _ := NewPacketizer(9).Packetize(1, make([]byte, 10))
	pkts2[0].Version = 0
	if _, _, err := Depacketize(pkts2); err == nil {
		t.Fatal("bad rtp version must fail")
	}
}

func TestEmptyPayloadRejected(t *testing.T) {
	if _, err := NewPacketizer(1).Packetize(1, nil); err == nil {
		t.Fatal("empty payload must be rejected")
	}
}
