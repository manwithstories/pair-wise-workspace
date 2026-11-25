package ingest

import (
	"bytes"
	"testing"

	"github.com/pairwise/mediapack/internal/transport"
)

func TestRTPTSRoundTrip(t *testing.T) {
	var ts bytes.Buffer
	for i := 0; i < 20; i++ {
		raw, err := transport.BuildPacket(transport.PacketOptions{
			PID: 0x100, CC: uint8(i % 16), PUSI: i == 0, Payload: []byte{byte(i), 1, 2, 3},
		})
		if err != nil {
			t.Fatal(err)
		}
		ts.Write(raw)
	}

	pz := &RTPPacketizer{SSRC: 73}
	pkts, err := pz.PacketizeTS(ts.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) != 3 { // 7 + 7 + 6
		t.Fatalf("RTP packets=%d, want 3", len(pkts))
	}
	dg, err := MarshalRTP(pkts)
	if err != nil {
		t.Fatal(err)
	}
	parsed, stats, err := ParseTSFromRTP(dg)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TSPackets != 20 || len(parsed) != 20 {
		t.Fatalf("stats=%+v parsed=%d", stats, len(parsed))
	}
	for i, p := range parsed {
		if p.PID != 0x100 || p.CC != uint8(i%16) {
			t.Fatalf("packet %d mismatch %+v", i, p)
		}
	}
}

func TestRTPPayloadMustBeTSAligned(t *testing.T) {
	pz := &RTPPacketizer{}
	if _, err := pz.PacketizeTS([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected non-aligned TS error")
	}
}
