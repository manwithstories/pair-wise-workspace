package rtpts

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/pion/rtp"

	"cinepipe/internal/fault"
)

// packet marshals an RTP packet with one 188-byte TS packet as payload.
func packet(t *testing.T, seq uint16, tsPayload []byte) []byte {
	t.Helper()
	p := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    33, // MP2T
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * 900,
			SSRC:           0x1234,
		},
		Payload: tsPayload,
	}
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// capture wraps RTP packets into the length-prefixed capture format.
func capture(rtpPackets ...[]byte) []byte {
	var b bytes.Buffer
	b.WriteString(Magic)
	for _, p := range rtpPackets {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(p)))
		b.Write(n[:])
		b.Write(p)
	}
	return b.Bytes()
}

func tsBytes(n int) []byte { return bytes.Repeat([]byte{0x47}, n) }

func TestCaptureRoundTripInOrder(t *testing.T) {
	data := capture(packet(t, 1, tsBytes(188)), packet(t, 2, tsBytes(188)))
	s, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if s.Stats.Packets != 2 || s.Stats.Duplicates != 0 || s.Stats.Lost != 0 {
		t.Fatalf("stats %+v", s.Stats)
	}
	if len(s.Bytes) != 376 {
		t.Fatalf("ts bytes %d", len(s.Bytes))
	}
}

func TestReorderRestored(t *testing.T) {
	// Arrival order 1,3,2,4: packet 3 arrives before 2.
	data := capture(
		packet(t, 1, tsBytes(188)),
		packet(t, 3, bytes.Repeat([]byte{0x03}, 188)),
		packet(t, 2, bytes.Repeat([]byte{0x02}, 188)),
		packet(t, 4, tsBytes(188)),
	)
	s, _ := Parse(data)
	if s.Stats.Reordered != 1 {
		t.Fatalf("reordered %d", s.Stats.Reordered)
	}
	// Reconstructed payload byte 188..376 should be seq 2's payload, then 3's.
	if s.Bytes[188] != 0x02 || s.Bytes[376] != 0x03 {
		t.Fatalf("order not restored: %02x %02x", s.Bytes[188], s.Bytes[376])
	}
	if len(s.Reorders) != 1 || s.Reorders[0].Seq != 2 {
		t.Fatalf("reorders %+v", s.Reorders)
	}
}

func TestDuplicateDropped(t *testing.T) {
	data := capture(
		packet(t, 1, tsBytes(188)),
		packet(t, 2, tsBytes(188)),
		packet(t, 2, bytes.Repeat([]byte{0xAA}, 188)),
		packet(t, 3, tsBytes(188)),
	)
	s, _ := Parse(data)
	if s.Stats.Duplicates != 1 {
		t.Fatalf("duplicates %d", s.Stats.Duplicates)
	}
	if len(s.Bytes) != 564 {
		t.Fatalf("duplicate payload included: %d", len(s.Bytes))
	}
}

func TestLossGap(t *testing.T) {
	// Sequence 1,2,3 then 6: three packets (4,5 and part of 6 skipped) lost two.
	data := capture(
		packet(t, 1, tsBytes(188)),
		packet(t, 2, tsBytes(188)),
		packet(t, 3, tsBytes(188)),
		packet(t, 6, tsBytes(188)),
	)
	s, _ := Parse(data)
	if s.Stats.Lost != 2 {
		t.Fatalf("lost %d", s.Stats.Lost)
	}
	if len(s.Gaps) != 1 || s.Gaps[0].Count != 2 || s.Gaps[0].Before != 3 || s.Gaps[0].After != 6 {
		t.Fatalf("gaps %+v", s.Gaps)
	}
}

func TestTruncatedRecord(t *testing.T) {
	// Build a capture then chop the final record short.
	good := capture(packet(t, 1, tsBytes(188)))
	chopped := good[:len(good)-10]
	s, err := Parse(chopped)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range s.Errors {
		if e.Kind == fault.KindRTP && bytes.Contains([]byte(e.Detail), []byte("claims")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("truncated record not reported: %v", s.Errors)
	}
}

func TestMalformedRTPpSkipped(t *testing.T) {
	// A record too short to be a valid RTP header (needs >= 12 bytes).
	var b bytes.Buffer
	b.WriteString(Magic)
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], 4)
	b.Write(n[:])
	b.Write([]byte{0x80, 0x21, 0x00, 0x01})
	// A good packet follows.
	b.Write(capture(packet(t, 1, tsBytes(188)))[len(Magic):])

	s, err := Parse(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if s.Stats.Packets != 1 {
		t.Fatalf("packets %d", s.Stats.Packets)
	}
	if len(s.Errors) != 1 || s.Errors[0].Kind != fault.KindRTP {
		t.Fatalf("errors %v", s.Errors)
	}
}

func TestMissingMagic(t *testing.T) {
	if _, err := Parse([]byte("not a capture")); err != ErrMagic {
		t.Fatalf("expected ErrMagic, got %v", err)
	}
}

func TestEmptyCapture(t *testing.T) {
	s, err := Parse([]byte(Magic))
	if err != nil {
		t.Fatal(err)
	}
	if s.Stats.Packets != 0 || len(s.Bytes) != 0 {
		t.Fatalf("empty capture: %+v", s.Stats)
	}
}
