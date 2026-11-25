package rtptrace

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/pion/rtp"
)

func recordLine(t *testing.T, seq uint16, ts uint32, payload []byte) string {
	t.Helper()
	p := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           0x42434153,
			Marker:         seq%2 == 0,
		},
		Payload: payload,
	}
	raw, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return `{"rtp_b64":"` + base64.StdEncoding.EncodeToString(raw) + `"}` + "\n"
}

func TestParseCountsDuplicateAndTimestampWrap(t *testing.T) {
	var b strings.Builder
	b.WriteString(recordLine(t, 100, 0xFFFFFFF0, []byte("a")))
	b.WriteString(recordLine(t, 101, 0xFFFFFFFA, []byte("b")))
	b.WriteString(recordLine(t, 101, 0xFFFFFFFA, []byte("b-dup")))
	b.WriteString(recordLine(t, 102, 0x00000004, []byte("c")))

	rep, err := Parse(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Packets != 4 {
		t.Fatalf("packets=%d want 4", rep.Packets)
	}
	if rep.SequenceDuplicates != 1 {
		t.Fatalf("duplicates=%d want 1", rep.SequenceDuplicates)
	}
	if rep.TimestampWraps != 1 {
		t.Fatalf("wraps=%d want 1", rep.TimestampWraps)
	}
	if !rep.Events[2].DuplicateSequence {
		t.Fatal("third packet must be marked duplicate")
	}
	// Parser must preserve the duplicated packet rather than silently drop it.
	if rep.Events[2].PayloadSize != len("b-dup") {
		t.Fatalf("duplicate payload size=%d", rep.Events[2].PayloadSize)
	}
}

func TestParseRejectsMalformedPacket(t *testing.T) {
	_, err := Parse(strings.NewReader("{\"rtp_b64\":\"not-valid-rtp\"}\n"))
	if err == nil {
		t.Fatal("expected malformed RTP error")
	}
}
