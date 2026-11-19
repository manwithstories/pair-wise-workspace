package packet

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

func rtpBytes(t *testing.T, seq uint16) []byte {
	t.Helper()
	p := &rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: 1000, SSRC: 7},
		Payload: []byte{1, 2, 3},
	}
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	sess := []byte(`{"tracks":[{"ssrc":7,"rate":90000,"kind":"video"}]}`)
	if err := WriteRecord(&buf, KindSession, sess); err != nil {
		t.Fatal(err)
	}
	if err := WriteRecord(&buf, KindRTP, rtpBytes(t, 42)); err != nil {
		t.Fatal(err)
	}
	sr := &rtcp.SenderReport{SSRC: 7, NTPTime: 1 << 32, RTPTime: 1000}
	rawSR, err := sr.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRecord(&buf, KindRTCP, rawSR); err != nil {
		t.Fatal(err)
	}
	if err := WriteRecord(&buf, KindTrunc, []byte{0x80, 0x60}); err != nil {
		t.Fatal(err)
	}

	rd := NewReader(&buf)
	r0, err := rd.Next()
	if err != nil || r0.Session == nil || r0.Session.Tracks[0].ClockRate != 90000 {
		t.Fatalf("session record: %v %+v", err, r0)
	}
	r1, err := rd.Next()
	if err != nil || r1.RTP.SequenceNumber != 42 {
		t.Fatalf("rtp record: %v %+v", err, r1)
	}
	r2, err := rd.Next()
	if err != nil || r2.SR == nil || r2.SR.RTPTime != 1000 {
		t.Fatalf("sr record: %v %+v", err, r2)
	}
	r3, err := rd.Next()
	if err != nil || !r3.Truncated {
		t.Fatalf("trunc record: %v %+v", err, r3)
	}
	if _, err := rd.Next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
	if r3.Index != 3 {
		t.Fatalf("record index = %d, want 3", r3.Index)
	}
}

func TestTruncatedPayloadRejected(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{KindRTP, 0, 10, 0x80}) // declares 10 bytes, provides 1
	rd := NewReader(&buf)
	if _, err := rd.Next(); err == nil {
		t.Fatal("expected error for truncated payload")
	}
}

func TestBadRTPRejected(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRecord(&buf, KindRTP, []byte{0xff}); err != nil {
		t.Fatal(err)
	}
	rd := NewReader(&buf)
	if _, err := rd.Next(); err == nil {
		t.Fatal("expected error for bad RTP packet")
	}
}

func TestRecordLimit(t *testing.T) {
	rd := NewReader(&bytes.Buffer{})
	rd.n = MaxRecords
	if _, err := rd.Next(); !errors.Is(err, ErrLimit) {
		t.Fatalf("want ErrLimit, got %v", err)
	}
}
