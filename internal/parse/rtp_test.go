package parse

import (
	"testing"

	"avscan/internal/model"
)

func packetRecords(t *testing.T, seqs []uint16) []RTPPacketRecord {
	t.Helper()
	const ssrc = 4242
	recs := make([]RTPPacketRecord, 0, len(seqs))
	for i, seq := range seqs {
		raw, err := BuildRTPPacket(ssrc, seq, uint32(i)*160, []byte{0xAA, 0xBB}, i == len(seqs)-1)
		if err != nil {
			t.Fatalf("BuildRTPPacket: %v", err)
		}
		recs = append(recs, RTPPacketRecord{
			StreamName: "voice", ClockRate: 8000, Raw: raw,
		})
	}
	return recs
}

func TestRTPHealthyAndWrap(t *testing.T) {
	// 65534, 65535, 0, 1 is a clean wrap with no gap.
	recs := packetRecords(t, []uint16{65534, 65535, 0, 1})
	res, err := ParseRTPStream(recs)
	if err != nil {
		t.Fatal(err)
	}
	if res.PacketCount != 4 || res.GapCount != 0 || len(res.Faults) != 0 {
		t.Errorf("wrap misdetected: count=%d gaps=%d faults=%d",
			res.PacketCount, res.GapCount, len(res.Faults))
	}
	if res.FirstSeq != 65534 || res.LastSeq != 1 {
		t.Errorf("seq bounds = %d..%d", res.FirstSeq, res.LastSeq)
	}
	if res.Events[0].StartNS != 0 {
		t.Errorf("first event must start at 0, got %d", res.Events[0].StartNS)
	}
	// 160 ticks at 8 kHz = 20 ms per packet.
	if res.Events[1].StartNS != 20_000_000 {
		t.Errorf("second start = %d, want 20000000", res.Events[1].StartNS)
	}
}

func TestRTPSequenceGap(t *testing.T) {
	recs := packetRecords(t, []uint16{10, 11, 14, 15})
	res, err := ParseRTPStream(recs)
	if err != nil {
		t.Fatal(err)
	}
	if res.GapCount != 2 {
		t.Errorf("gap count = %d, want 2", res.GapCount)
	}
	if len(res.Faults) != 1 || res.Faults[0].Kind != model.FaultRTPSeqGap {
		t.Fatalf("faults = %+v, want one seq-gap fault", res.Faults)
	}
	if !res.Faults[0].HasTime || res.Faults[0].SampleIndex != 2 {
		t.Errorf("gap fault must carry time and packet index: %+v", res.Faults[0])
	}
}

func TestRTPMalformedBoundary(t *testing.T) {
	good := packetRecords(t, []uint16{5})
	recs := []RTPPacketRecord{
		good[0],
		{StreamName: "voice", ClockRate: 8000, Raw: []byte{0x80}}, // truncated header
	}
	res, err := ParseRTPStream(recs)
	if err != nil {
		t.Fatal(err)
	}
	if res.PacketCount != 1 {
		t.Errorf("valid packet count = %d, want 1", res.PacketCount)
	}
	if len(res.Faults) != 1 || res.Faults[0].Kind != model.FaultRTPBoundary {
		t.Fatalf("faults = %+v, want one boundary fault", res.Faults)
	}
}

func TestRTPEmptyRejected(t *testing.T) {
	if _, err := ParseRTPStream(nil); err == nil {
		t.Error("empty stream must error")
	}
}
