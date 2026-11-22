package stream

import (
	"testing"

	"mediascan/internal/fixture"
	"mediascan/internal/media"
)

// rtpPacket delegates to the shared RTP fixture.
func rtpPacket(t *testing.T, seq uint16, ts uint32, payloadLen int) []byte {
	t.Helper()
	return fixture.RTPPacket(seq, ts, payloadLen)
}

func TestAnalyzeRTPSequenceWrap(t *testing.T) {
	var pkts [][]byte
	seq := uint16(65533)
	for i := 0; i < 6; i++ {
		pkts = append(pkts, rtpPacket(t, seq, uint32(i)*3000, 160))
		seq++ // wraps naturally through 0
	}
	a := AnalyzeRTP(pkts, 90000, RTPLimits{})
	if len(a.Packets) != 6 {
		t.Fatalf("packets = %d", len(a.Packets))
	}
	if a.Wraps == 0 {
		t.Errorf("wrap not counted; issues=%v", a.Issues)
	}
	if !hasClass(a.Issues, media.ClassRTPSequenceWrap) {
		t.Errorf("no wrap issue: %v", a.Issues)
	}
	// Wrap is informational, not a failure.
	if a.Issues.Fatal() {
		t.Error("wrap reported as fatal")
	}
	// Timestamps scaled to ns at 90kHz.
	if a.Packets[1].TimestampNs != 33_333_333 {
		t.Errorf("ts ns = %d, want 33333333", a.Packets[1].TimestampNs)
	}
}

func TestAnalyzeRTPDuplicateAndOutOfOrder(t *testing.T) {
	pkts := [][]byte{
		rtpPacket(t, 100, 0, 160),
		rtpPacket(t, 101, 3000, 160),
		rtpPacket(t, 101, 3000, 160), // duplicate
		rtpPacket(t, 100, 0, 160),    // late duplicate from earlier
		rtpPacket(t, 99, 0, 160),     // out of order
		rtpPacket(t, 102, 6000, 160),
	}
	a := AnalyzeRTP(pkts, 0, RTPLimits{})
	if a.Duplicates != 2 {
		t.Errorf("duplicates = %d, want 2", a.Duplicates)
	}
	if a.OutOfOrder < 1 {
		t.Errorf("out-of-order = %d", a.OutOfOrder)
	}
	if !hasClass(a.Issues, media.ClassRTPDuplicate) || !hasClass(a.Issues, media.ClassRTPOutOfOrder) {
		t.Errorf("missing classes: %v", a.Issues)
	}
}

func TestAnalyzeRTPTruncated(t *testing.T) {
	good := rtpPacket(t, 1, 0, 160)
	truncated := good[:5] // shorter than a minimal RTP header
	pkts := [][]byte{good, truncated, rtpPacket(t, 3, 6000, 160)}
	a := AnalyzeRTP(pkts, 90000, RTPLimits{})
	if a.Truncated != 1 {
		t.Fatalf("truncated = %d", a.Truncated)
	}
	is := media.Issue{}
	found := false
	for _, x := range a.Issues {
		if x.Class == media.ClassRTPTruncated {
			is, found = x, true
		}
	}
	if !found {
		t.Fatalf("no truncated issue: %v", a.Issues)
	}
	if is.Locator.PacketIndex != 1 {
		t.Errorf("truncated located at packet %d, want 1", is.Locator.PacketIndex)
	}
	// Two intact runs: [0,1) and [2,3).
	if len(a.Recovered) != 2 {
		t.Fatalf("recovered ranges = %+v, want 2", a.Recovered)
	}
	if a.Recovered[0].PacketStart != 0 || a.Recovered[0].PacketEnd != 1 {
		t.Errorf("range0 = %+v", a.Recovered[0])
	}
	if a.Recovered[1].PacketStart != 2 || a.Recovered[1].PacketEnd != 3 {
		t.Errorf("range1 = %+v", a.Recovered[1])
	}
	if a.Issues.Fatal() {
		t.Error("truncated packet should be recoverable, not fatal")
	}
}

func TestAnalyzeRTPLimits(t *testing.T) {
	pkts := make([][]byte, 10)
	for i := range pkts {
		pkts[i] = rtpPacket(t, uint16(i), uint32(i)*3000, 160)
	}
	a := AnalyzeRTP(pkts, 90000, RTPLimits{MaxPackets: 5})
	if !a.Issues.Fatal() {
		t.Error("packet-count overflow should be fatal")
	}
	if len(a.Packets) != 0 {
		t.Errorf("packets parsed past limit: %d", len(a.Packets))
	}
	big := rtpPacket(t, 1, 0, 200)
	a = AnalyzeRTP([][]byte{big}, 90000, RTPLimits{MaxPayload: 10})
	if !hasClass(a.Issues, media.ClassRTPInvalidHeader) {
		t.Errorf("oversized payload not reported: %v", a.Issues)
	}
}

func TestAnalyzeRTPDeterministic(t *testing.T) {
	pkts := [][]byte{
		rtpPacket(t, 5, 0, 160), rtpPacket(t, 6, 3000, 160),
		rtpPacket(t, 6, 3000, 160), rtpPacket(t, 1, 9000, 160),
	}
	a1 := AnalyzeRTP(pkts, 90000, RTPLimits{})
	a2 := AnalyzeRTP(pkts, 90000, RTPLimits{})
	if len(a1.Issues) != len(a2.Issues) {
		t.Fatalf("issue counts differ: %d vs %d", len(a1.Issues), len(a2.Issues))
	}
	for i := range a1.Issues {
		if a1.Issues[i].Error() != a2.Issues[i].Error() {
			t.Errorf("issue %d differs:\n%s\n%s", i, a1.Issues[i].Error(), a2.Issues[i].Error())
		}
	}
}
