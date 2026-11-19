package clock

import (
	"testing"

	"github.com/pion/rtp"
)

func TestRTPExtensionRoundTrip(t *testing.T) {
	// Pick an instant whose RTP timestamp would wrap many times: the
	// extension keeps the full precision while the 32-bit field cannot.
	tick := Tick(int64(1<<32)*3 + TickRate*7)
	pkt, err := EncodeRTPPacket(0xDEADBEEF, 4242, tick)
	if err != nil {
		t.Fatal(err)
	}
	if pkt.SSRC != 0xDEADBEEF || pkt.SequenceNumber != 4242 || !pkt.Extension {
		t.Fatalf("header fields wrong: %+v", pkt.Header)
	}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRTPPacket(raw)
	if err != nil || got != tick {
		t.Fatalf("extension roundtrip=%d want %d err=%v", got, tick, err)
	}
}

func TestRTPWithoutExtensionFallsBack(t *testing.T) {
	pkt := &rtp.Packet{
		Header: rtp.Header{Version: 2, PayloadType: 96, Timestamp: 90000},
	}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRTPPacket(raw)
	if err != nil || got != Tick(TickRate) {
		t.Fatalf("fallback decode=%d want %d err=%v", got, TickRate, err)
	}
}

func TestRTPPacketTimestampAlignment(t *testing.T) {
	// Extension and the plain RTP timestamp must agree on the shared instant.
	tick := MillisToTick(1500)
	pkt, err := EncodeRTPPacket(1, 1, tick)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := TickToRTP(tick, RTPClockRate); pkt.Timestamp != want {
		t.Fatalf("ts=%d want %d", pkt.Timestamp, want)
	}
	raw, _ := pkt.Marshal()
	back, err := DecodeRTPPacket(raw)
	if err != nil || back != tick {
		t.Fatalf("align decode=%d want %d err=%v", back, tick, err)
	}
}
