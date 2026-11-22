package rtp

import "testing"

func TestWindowRejectsTooLate(t *testing.T) {
	// Lose packet 0, then retransmit it after the receiver has already
	// advanced well past the window.
	// Packets 0..99 arrive in order, then packet 100, then the repair
	// stream finally supplies packet 0 -- far outside a 16-slot window.
	var obs []Packet
	arrive := 0
	for seq := 0; seq <= 100; seq++ {
		if seq == 0 {
			continue
		}
		obs = append(obs, Packet{Seq: uint16(seq), Arrive: arrive, RTP: newPacket(uint16(seq))})
		arrive++
	}
	obs = append(obs, Packet{Seq: 0, Arrive: arrive, Retransmit: true, RTP: newPacket(0)})
	res, err := RecoverWindow(101, obs, 16)
	if err != nil {
		t.Fatal(err)
	}
	if res.TooLate != 1 || res.Recovered != 0 || res.Unrecoverable != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestWindowAcceptsInsideWindow(t *testing.T) {
	// Packet 5 is lost; the repair stream supplies it after packet 6
	// arrives, well inside a 16-slot window.
	obs := []Packet{
		{Seq: 3, Arrive: 0, RTP: newPacket(3)},
		{Seq: 4, Arrive: 1, RTP: newPacket(4)},
		{Seq: 6, Arrive: 2, RTP: newPacket(6)},
		{Seq: 5, Arrive: 3, Retransmit: true, RTP: newPacket(5)},
		{Seq: 7, Arrive: 4, RTP: newPacket(7)},
	}
	res, err := RecoverWindow(8, obs, 16)
	if err != nil {
		t.Fatal(err)
	}
	if res.TooLate != 0 || res.Recovered != 1 || res.Unrecoverable != 3 {
		t.Fatalf("result = %+v", res)
	}
}

func TestWindowBounds(t *testing.T) {
	if _, err := RecoverWindow(4, nil, 0); err == nil {
		t.Fatal("zero window must be rejected")
	}
	if _, err := RecoverWindow(4, nil, 1<<17); err == nil {
		t.Fatal("oversized window must be rejected")
	}
}
