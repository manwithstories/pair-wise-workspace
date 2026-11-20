package rtp

import (
	"testing"

	"archive/internal/testdata"
)

// TestParseRTP verifies a generated RTP datagram parses with its fields.
func TestParseRTP(t *testing.T) {
	spec := testdata.TrackSpec{SSRC: 0x1234, ClockRate: 90000, PayloadPT: 96,
		Packets: 1, KeyEvery: 0, TSInterval: 900, Kind: "video"}
	raw, _, err := testdata.BuildPacket(spec, 5)
	if err != nil {
		t.Fatal(err)
	}
	dg, err := ParseDatagram(raw)
	if err != nil {
		t.Fatal(err)
	}
	if dg.Kind != KindRTP {
		t.Fatal("expected RTP datagram")
	}
	if dg.Packet.SSRC != 0x1234 {
		t.Errorf("ssrc = %#x", dg.Packet.SSRC)
	}
	if dg.Packet.Sequence != 5 {
		t.Errorf("sequence = %d, want 5", dg.Packet.Sequence)
	}
	if len(dg.Packet.Payload) == 0 {
		t.Error("payload should be present")
	}
}

// TestParseRTCP verifies a Sender Report decodes and stays valid.
func TestParseRTCP(t *testing.T) {
	raw := testdata.BuildSenderReport(0x99, 1000, 3898444800<<32, 10, 640)
	dg, err := ParseDatagram(raw)
	if err != nil {
		t.Fatal(err)
	}
	if dg.Kind != KindRTCP {
		t.Fatal("expected RTCP datagram")
	}
	if dg.Report.SSRC != 0x99 || !dg.Report.Valid {
		t.Errorf("bad report: %+v", dg.Report)
	}
	if dg.Report.RTPTime != 1000 {
		t.Errorf("rtp time = %d, want 1000", dg.Report.RTPTime)
	}
}

// TestParseTruncatedRejects verifies short/corrupt datagrams are rejected.
func TestParseTruncatedRejects(t *testing.T) {
	for _, raw := range [][]byte{{}, {0x80}, {0x80, 200, 0, 6}} {
		if _, err := ParseDatagram(raw); err == nil {
			t.Errorf("expected error for %d-byte datagram", len(raw))
		}
	}
}

// TestParseCorruptRTCPFlagged verifies a zeroed-NTP report is parsed but
// marked invalid rather than dropped.
func TestParseCorruptRTCPFlagged(t *testing.T) {
	raw := testdata.BuildSenderReport(1, 0, 0, 0, 0)
	dg, err := ParseDatagram(raw)
	if err != nil {
		t.Fatal(err)
	}
	if dg.Report.Valid {
		t.Error("zeroed NTP report must be flagged invalid")
	}
}
