package recovery

import (
	"math/rand"
	"testing"

	"github.com/pion/rtp"

	"mediad/internal/media"
)

func packet(seq uint16, ts uint32, payload string) *rtp.Packet {
	p := &rtp.Packet{Header: rtp.Header{
		Version:        2,
		SequenceNumber: seq,
		Timestamp:      ts,
		SSRC:           0xABCD,
		PayloadType:    96,
	}}
	p.Payload = []byte(payload)
	return p
}

func TestUnwrapSequenceWrap(t *testing.T) {
	// Sequence numbers crossing the 16-bit boundary must stay monotonic.
	sent := make([]uint16, 5)
	for i := range sent {
		sent[i] = uint16(0xFFFE + i) // FFFE, FFFF, 0000, 0001, 0002
	}
	got := unwrapAll(sent)
	want := []int64{65534, 65535, 65536, 65537, 65538}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unwrap[%d]=%d want %d", i, got[i], want[i])
		}
	}
}

func TestReorderAndDedup(t *testing.T) {
	sent := make([]uint16, 6)
	in := make([]*rtp.Packet, 0, 7)
	payloads := map[uint16]string{10: "a", 11: "b", 12: "c", 13: "d", 14: "e", 15: "f"}
	for i := range sent {
		sent[i] = uint16(10 + i)
	}
	// Deliberately out of order plus a duplicate of sequence 12.
	in = append(in, packet(13, 3000, "d"))
	in = append(in, packet(10, 0, "a"))
	in = append(in, packet(12, 2000, "c"))
	in = append(in, packet(12, 2000, "c")) // duplicate
	in = append(in, packet(11, 1000, "b"))
	in = append(in, packet(15, 5000, "f"))
	in = append(in, packet(14, 4000, "e"))

	res, err := Recover(0xABCD, "cam1", in, nil, Options{SentSeqs: sent})
	if err != nil {
		t.Fatal(err)
	}
	if res.DupCount != 1 {
		t.Fatalf("duplicates=%d want 1", res.DupCount)
	}
	if res.LostCount != 0 || len(res.Packets) != 6 {
		t.Fatalf("lost=%d packets=%d", res.LostCount, len(res.Packets))
	}
	for i := 1; i < len(res.Packets); i++ {
		if res.Packets[i-1].Seq >= res.Packets[i].Seq {
			t.Fatalf("packets not ordered: %v", res.Packets)
		}
	}
	for _, p := range res.Packets {
		if string(p.Payload) != payloads[p.Wire] {
			t.Fatalf("seq %d payload %q want %q", p.Wire, p.Payload, payloads[p.Wire])
		}
	}
	if got := res.DeliveryPct(); got != 100 {
		t.Fatalf("delivery=%.2f want 100", got)
	}
}

func TestRecoverFromRedundantCopy(t *testing.T) {
	// Sequence 12 is lost; packet 13 redundantly carries its payload.
	sent := []uint16{10, 11, 12, 13, 14}
	in := []*rtp.Packet{
		packet(10, 0, "a"),
		packet(11, 1000, "b"),
		packet(13, 2000, "c"), // carries copy of 12
		packet(14, 4000, "e"),
	}
	red := Redundancy{13: []RepairCopy{{Orig: 12, Payload: []byte("c"), Timestamp: 2000}}}
	res, err := Recover(0xABCD, "cam1", in, red, Options{SentSeqs: sent})
	if err != nil {
		t.Fatal(err)
	}
	if res.RecoveredCount != 1 || len(res.Recovered) != 1 || res.Recovered[0] != 12 {
		t.Fatalf("recovered=%v count=%d", res.Recovered, res.RecoveredCount)
	}
	if res.LostCount != 0 {
		t.Fatalf("lost=%d want 0 after recovery", res.LostCount)
	}
	if got := res.RecoveryPct(); got != 100 {
		t.Fatalf("recovery=%.2f want 100", got)
	}
	var got Packet
	for _, p := range res.Packets {
		if p.Seq == 12 {
			got = p
		}
	}
	if !got.Recovered || string(got.Payload) != "c" {
		t.Fatalf("recovered packet wrong: %+v", got)
	}
}

func TestUnrecoverableLoss(t *testing.T) {
	sent := []uint16{1, 2, 3, 4, 5}
	in := []*rtp.Packet{
		packet(1, 0, "a"),
		// 2 and 3 lost with no redundancy
		packet(4, 3000, "d"),
		packet(5, 4000, "e"),
	}
	res, err := Recover(1, "cam1", in, nil, Options{SentSeqs: sent})
	if err != nil {
		t.Fatal(err)
	}
	if res.LostCount != 2 {
		t.Fatalf("lost=%d want 2", res.LostCount)
	}
	if res.Lost[0] != 2 || res.Lost[1] != 3 {
		t.Fatalf("lost list=%v", res.Lost)
	}
	if got := res.DeliveryPct(); got != 60 {
		t.Fatalf("delivery=%.2f want 60", got)
	}
}

func TestConflictingPayloadRejected(t *testing.T) {
	in := []*rtp.Packet{
		packet(7, 0, "a"),
		packet(7, 0, "b"), // same seq, different bytes
	}
	_, err := Recover(1, "cam1", in, nil, Options{SentSeqs: []uint16{7}})
	if err == nil {
		t.Fatal("expected conflicting-payload error")
	}
	if media.CodeOf(err) != media.CodeSegmentParse {
		t.Fatalf("code=%s", media.CodeOf(err))
	}
}

func TestPacketLimit(t *testing.T) {
	in := make([]*rtp.Packet, 5)
	for i := range in {
		in[i] = packet(uint16(i), uint32(i)*3000, "x")
	}
	_, err := Recover(1, "cam1", in, nil, Options{
		SentSeqs: []uint16{0, 1, 2, 3, 4},
		Limits:   media.Limits{MaxPackets: 2},
	})
	if err == nil || media.CodeOf(err) != media.CodeLimit {
		t.Fatalf("expected limit error, got %v", err)
	}
}

func TestRandomLossRecoveryRate(t *testing.T) {
	// End-to-end statistical check: with 3% loss and every-third redundancy,
	// at least 95% of dropped packets should be repaired.
	rng := rand.New(rand.NewSource(7))
	const n = 2000
	sent := make([]uint16, n)
	payloads := make(map[uint16][]byte, n)
	// Every carrier redundantly re-sends its predecessor's payload, as an
	// RTX/FEC scheme would; the receiver gets a second chance at each packet.
	copies := make(map[uint16][]RepairCopy)
	for i := 0; i < n; i++ {
		sent[i] = uint16(i)
		payloads[uint16(i)] = []byte{byte(i), byte(i >> 8), 0xAB}
	}
	drop := make(map[uint16]bool)
	for i := 0; i < n; i++ {
		for back := 1; back <= 2 && i-back >= 0; back++ {
			copies[uint16(i)] = append(copies[uint16(i)], RepairCopy{
				Orig:      uint16(i - back),
				Payload:   payloads[uint16(i-back)],
				Timestamp: uint32(i-back) * 3000,
			})
		}
		if rng.Float64() < 0.03 {
			drop[uint16(i)] = true
		}
	}
	var in []*rtp.Packet
	for i := 0; i < n; i++ {
		if drop[uint16(i)] {
			continue
		}
		in = append(in, packet(uint16(i), uint32(i)*3000, string(payloads[uint16(i)])))
	}
	res, err := Recover(1, "cam1", in, copies, Options{SentSeqs: sent})
	if err != nil {
		t.Fatal(err)
	}
	if res.RecoveredCount == 0 {
		t.Fatal("expected some recovered packets")
	}
	if res.RecoveryPct() < 95 {
		t.Fatalf("recovery=%.2f%% below 95%% (lost=%d recovered=%d)",
			res.RecoveryPct(), res.LostCount, res.RecoveredCount)
	}
}
