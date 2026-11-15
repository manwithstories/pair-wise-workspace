package fixture

import "testing"

func TestApplyKeepsSequenceJumpBounded(t *testing.T) {
	f, err := Generate(120)
	if err != nil {
		t.Fatal(err)
	}
	packets := Apply(f.Packetize(FirstSequence(7)), ImpairOptions{
		Loss: 0.02, Reorder: 0.01, Duplicate: 0.01, Seed: 7,
	})
	if len(packets) == 0 {
		t.Fatal("no packets")
	}
	prev := int(packets[0].RTP.SequenceNumber)
	for i, packet := range packets[1:] {
		seq := int(packet.RTP.SequenceNumber)
		if seq > prev+40 || seq < prev-40 {
			t.Fatalf("large sequence jump at %d: %d -> %d", i+1, prev, seq)
		}
		prev = seq
	}
}

func TestFixtureCoversAllPacketizationModes(t *testing.T) {
	f, err := Generate(10)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[byte]bool)
	for _, packet := range f.Packetize(FirstSequence(7)) {
		if len(packet.RTP.Payload) > 0 {
			seen[packet.RTP.Payload[0]&0x1f] = true
		}
	}
	if !seen[1] && !seen[5] {
		t.Fatal("fixture has no single-NAL payload")
	}
	if !seen[24] || !seen[28] {
		t.Fatalf("missing STAP-A/FU-A coverage: %v", seen)
	}
}
