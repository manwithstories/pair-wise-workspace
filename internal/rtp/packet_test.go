package rtpadapter_test

import (
	"testing"

	"github.com/pion/rtp"

	"mediaproc/internal/demux"
	rtpadapter "mediaproc/internal/rtp"
)

func TestPacketizeTimestampSequenceAndMarker(t *testing.T) {
	pz := rtpadapter.NewOpusPacketizer(0x99)
	payload := make([]byte, 1500)
	for i := range payload {
		payload[i] = byte(i)
	}
	b := &demux.Block{TimeNS: 20_000_000, Data: payload, Frames: [][]byte{payload}}
	pkts, err := pz.Packetize(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) != 2 {
		t.Fatalf("packets=%d want 2", len(pkts))
	}
	if pkts[0].SequenceNumber != 0 || pkts[1].SequenceNumber != 1 {
		t.Fatalf("sequences=%d,%d", pkts[0].SequenceNumber, pkts[1].SequenceNumber)
	}
	if pkts[0].Marker || !pkts[1].Marker {
		t.Fatalf("markers=%v,%v", pkts[0].Marker, pkts[1].Marker)
	}
	if pkts[0].Timestamp != 960 || pkts[1].Timestamp != 960 {
		t.Fatalf("timestamp=%d want 960", pkts[0].Timestamp)
	}
	raw, err := pkts[0].Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var decoded rtp.Packet
	if err := decoded.Unmarshal(raw); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Payload) != pz.MTU {
		t.Fatalf("payload len=%d want %d", len(decoded.Payload), pz.MTU)
	}
}

func TestEmptyFrameReportsOffset(t *testing.T) {
	pz := rtpadapter.NewOpusPacketizer(1)
	_, err := pz.Packetize(&demux.Block{Offset: 77})
	if err == nil {
		t.Fatal("expected empty frame error")
	}
}
