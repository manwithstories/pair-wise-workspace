package media

import (
	"bytes"
	"testing"
	"time"
)

func TestSourceDeterministicAndTimed(t *testing.T) {
	a := NewSource(Config{SSRC: 7, Seed: 42})
	b := NewSource(Config{SSRC: 7, Seed: 42})
	for i := 0; i < 100; i++ {
		pa, pb := a.Next(), b.Next()
		if pa.SequenceNumber != uint16(i) || pa.Timestamp != uint32(i*960) {
			t.Fatalf("unexpected timing at %d: %+v", i, pa.Header)
		}
		if !bytes.Equal(pa.Payload, pb.Payload) {
			t.Fatalf("same seed diverged at packet %d", i)
		}
	}
	if PacketCount(12*time.Second, DefaultFrameDuration) != 600 {
		t.Fatal("unexpected packet count")
	}
}

func TestSourceSequenceWrap(t *testing.T) {
	src := NewSource(Config{SSRC: 7, Seed: 42})
	for i := 0; i < 65537; i++ {
		packet := src.Next()
		if i == 65535 && packet.SequenceNumber != 65535 {
			t.Fatalf("expected 65535, got %d", packet.SequenceNumber)
		}
		if i == 65536 && packet.SequenceNumber != 0 {
			t.Fatalf("expected wrap to 0, got %d", packet.SequenceNumber)
		}
	}
}
