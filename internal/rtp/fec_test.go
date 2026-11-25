package rtp

import (
	"bytes"
	"testing"

	pionrtp "github.com/pion/rtp"
)

func TestFECRecoversSingleLoss(t *testing.T) {
	orig := []*pionrtp.Packet{
		mediaPkt(0, 0, 10), mediaPkt(1, 3000, 20),
		mediaPkt(2, 6000, 30), mediaPkt(3, 9000, 40),
	}
	stream := EncodeFEC(orig)
	if len(stream) != 5 {
		t.Fatalf("encoded=%d, want 5 (4 media + 1 parity)", len(stream))
	}
	parity := stream[4]
	base, size, ok := ParseParity(parity)
	if !ok || base != 0 || size != 4 {
		t.Fatalf("parity header wrong: base=%d size=%d ok=%v", base, size, ok)
	}
	// Lose seq 2; keep the other three plus parity.
	group := FECGroup{
		Base: 0, Size: 4, Parity: parity,
		Present: map[uint16]*pionrtp.Packet{
			0: stream[0], 1: stream[1], 3: stream[3],
		},
	}
	recovered, unrecoverable := DecodeFEC([]FECGroup{group})
	if unrecoverable != 0 {
		t.Fatalf("unrecoverable=%d, want 0", unrecoverable)
	}
	got, ok := recovered[2]
	if !ok {
		t.Fatal("seq 2 not recovered")
	}
	if !bytes.Equal(got.Payload, orig[2].Payload) {
		t.Fatalf("recovered payload=%v, want %v", got.Payload, orig[2].Payload)
	}
}

func TestFECCannotRecoverDoubleLoss(t *testing.T) {
	orig := []*pionrtp.Packet{
		mediaPkt(0, 0, 1), mediaPkt(1, 3000, 2),
		mediaPkt(2, 6000, 3), mediaPkt(3, 9000, 4),
	}
	stream := EncodeFEC(orig)
	group := FECGroup{
		Base: 0, Size: 4, Parity: stream[4],
		Present: map[uint16]*pionrtp.Packet{0: stream[0], 2: stream[2]},
	}
	_, unrecoverable := DecodeFEC([]FECGroup{group})
	if unrecoverable != 1 {
		t.Fatalf("unrecoverable=%d, want 1 for double loss", unrecoverable)
	}
}

func TestFECNoLossNoWork(t *testing.T) {
	orig := []*pionrtp.Packet{mediaPkt(0, 0, 1), mediaPkt(1, 3000, 2)}
	stream := EncodeFEC(orig)
	base, _, _ := ParseParity(stream[2])
	group := FECGroup{
		Base: base, Size: 2, Parity: stream[2],
		Present: map[uint16]*pionrtp.Packet{0: stream[0], 1: stream[1]},
	}
	recovered, unrecoverable := DecodeFEC([]FECGroup{group})
	if len(recovered) != 0 || unrecoverable != 0 {
		t.Fatalf("intact group must need no recovery: rec=%d unrec=%d", len(recovered), unrecoverable)
	}
}

func TestFECMarshalRoundTrip(t *testing.T) {
	orig := []*pionrtp.Packet{mediaPkt(7, 21000, 99)}
	stream := EncodeFEC(orig)
	raw, err := stream[1].Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var got pionrtp.Packet
	if err := got.Unmarshal(raw); err != nil {
		t.Fatal(err)
	}
	if got.PayloadType != PayloadTypeParity || got.SequenceNumber != 0xF000 {
		t.Fatalf("parity packet wire format wrong: type=%d seq=%d", got.PayloadType, got.SequenceNumber)
	}
}
