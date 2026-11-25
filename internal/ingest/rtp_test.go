package ingest

import (
	"testing"

	"github.com/pion/rtp"

	"fieldcut/internal/clock"
)

func marshalRTP(t *testing.T, seq uint16, ts uint32, payload string) []byte {
	t.Helper()
	b, err := (&rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: 17},
		Payload: []byte(payload),
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseRTPWrapReorderAndMissing(t *testing.T) {
	// Sequence crosses 65535 -> 0; timestamp crosses uint32 max -> 0.
	raw := [][]byte{
		marshalRTP(t, 65535, 0xfffffff0, "a"),
		marshalRTP(t, 2, 0x00000010, "c"), // out of order and missing seq 0/1 after wrap
		marshalRTP(t, 0, 0x00000000, "b"),
	}
	samples, reports, err := ParseRTPPackets(7, 90000, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 3 {
		t.Fatalf("samples=%d", len(samples))
	}
	if samples[0].ExtendedSequence != 65535 || samples[1].ExtendedSequence != 65536 || samples[2].ExtendedSequence != 65538 {
		t.Fatalf("bad extended sequences: %+v", samples)
	}
	if samples[1].ExtendedTimestamp != 1<<32 {
		t.Fatalf("timestamp did not wrap: %d", samples[1].ExtendedTimestamp)
	}
	if samples[1].PTS != clock.NsPerSecond*16/90000 {
		t.Fatalf("bad wrapped PTS: %d", samples[1].PTS)
	}
	if len(reports) != 1 || reports[0].Code != clock.CodeMissingPacket {
		t.Fatalf("expected one missing report, got %#v", reports)
	}
	if samples[2].MissingBefore != 1 {
		t.Fatalf("missing count=%d", samples[2].MissingBefore)
	}
}

func TestExtendUint32ReorderDoesNotDoubleWrap(t *testing.T) {
	prev := uint64(1 << 32) // already wrapped once
	if got := ExtendUint32(prev, 0xfffffffe); got != 0xfffffffe {
		t.Fatalf("old timestamp extended to %d", got)
	}
	if got := ExtendUint32(prev, 2); got != 1<<32+2 {
		t.Fatalf("new timestamp extended to %d", got)
	}
}
