package rtp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"archiver/internal/media"
)

// buildRTP assembles a minimal RTP packet for fixtures.
func buildRTP(seq uint16, ts, ssrc uint32, payload []byte) []byte {
	b := make([]byte, rtpHeaderLen+len(payload))
	b[0] = 0x80 // V=2
	b[1] = 96
	binary.BigEndian.PutUint16(b[2:4], seq)
	binary.BigEndian.PutUint32(b[4:8], ts)
	binary.BigEndian.PutUint32(b[8:12], ssrc)
	copy(b[rtpHeaderLen:], payload)
	return b
}

func pktErrOffset(t *testing.T, err error) int64 {
	t.Helper()
	var pe *media.PacketError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *media.PacketError, got %T (%v)", err, err)
	}
	return pe.Offset
}

func TestParseValidRoundTrip(t *testing.T) {
	raw := buildRTP(7, 12345, 0xdeadbeef, []byte{1, 2, 3, 4})
	p, err := ParsePacket(raw, 100)
	if err != nil {
		t.Fatal(err)
	}
	if p.Seq != 7 || p.Timestamp != 12345 || p.SSRC != 0xdeadbeef {
		t.Fatalf("bad fields: %+v", p)
	}
	if !bytes.Equal(p.Serialize(), raw) {
		t.Fatal("round trip mismatch")
	}
}

func TestParseShortHeader(t *testing.T) {
	raw := buildRTP(1, 0, 1, nil)[:8] // 8 of 12 header bytes
	err := mustErr(t, raw)
	if got := pktErrOffset(t, err); got != 8 {
		t.Fatalf("first anomalous byte = %d, want 8", got)
	}
}

func TestParseBadVersion(t *testing.T) {
	raw := buildRTP(1, 0, 1, nil)
	raw[0] = 0x40 // V=1
	if got := pktErrOffset(t, mustErr(t, raw)); got != 0 {
		t.Fatalf("first anomalous byte = %d, want 0", got)
	}
}

func TestParseTruncatedExtension(t *testing.T) {
	payload := []byte{9, 9}
	raw := buildRTP(1, 0, 1, payload)
	raw[0] = 0x90 // V=2, X=1
	// Extension header declares 2 words (8 bytes) but only 4 bytes of
	// extension data precede the payload; rebuild accordingly.
	ext := []byte{0xbe, 0xde, 0x00, 0x02, 1, 2, 3, 4}
	full := append(raw[:rtpHeaderLen], append(ext, payload...)...)
	_, err := ParsePacket(full, 0)
	if err == nil {
		t.Fatal("expected truncated extension error")
	}
	// need 12+4+8=24 bytes, have 22 -> first missing byte at 22.
	if got := pktErrOffset(t, err); got != 22 {
		t.Fatalf("first anomalous byte = %d, want 20", got)
	}
}

func TestParseExtensionRoundTrip(t *testing.T) {
	raw := buildRTP(3, 99, 5, []byte{7})
	raw[0] = 0x90
	ext := []byte{0x10, 0x00, 0x00, 0x01, 0xaa, 0xbb, 0xcc, 0xdd}
	full := append(raw[:rtpHeaderLen], append(ext, raw[rtpHeaderLen:]...)...)
	p, err := ParsePacket(full, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.ExtProfile != 0x1000 || !bytes.Equal(p.ExtData, []byte{0xaa, 0xbb, 0xcc, 0xdd}) {
		t.Fatalf("bad extension: %+v", p)
	}
	if !bytes.Equal(p.Serialize(), full) {
		t.Fatal("extension round trip mismatch")
	}
}

func TestParseSenderReport(t *testing.T) {
	b := make([]byte, rtcpSRLen)
	b[0] = 0x80
	b[1] = 200
	binary.BigEndian.PutUint32(b[4:8], 0x11223344)
	binary.BigEndian.PutUint32(b[8:12], 3900000000)
	binary.BigEndian.PutUint32(b[12:16], 1<<31)
	binary.BigEndian.PutUint32(b[16:20], 44100)
	p, err := ParsePacket(b, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !p.RTCP || p.SR.SSRC != 0x11223344 || p.SR.RTPTimestamp != 44100 {
		t.Fatalf("bad SR: %+v", p.SR)
	}
}

func mustErr(t *testing.T, raw []byte) error {
	t.Helper()
	_, err := ParsePacket(raw, 0)
	if err == nil {
		t.Fatal("expected error")
	}
	return err
}

func TestReaderIncremental(t *testing.T) {
	var stream bytes.Buffer
	for i := 0; i < 3; i++ {
		raw := buildRTP(uint16(i), uint32(i*10), 1, []byte{byte(i)})
		var pre [4]byte
		binary.BigEndian.PutUint32(pre[:], uint32(len(raw)))
		stream.Write(pre[:])
		stream.Write(raw)
	}
	rd := NewReader(&stream, 0)
	for i := 0; i < 3; i++ {
		p, err := rd.Next()
		if err != nil {
			t.Fatal(err)
		}
		if p.Seq != uint16(i) {
			t.Fatalf("seq = %d, want %d", p.Seq, i)
		}
		wantOff := int64(i * (4 + rtpHeaderLen + 1))
		if p.Offset != wantOff+4 {
			t.Fatalf("offset = %d, want %d", p.Offset, wantOff+4)
		}
	}
	if _, err := rd.Next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestReaderOversizeAndTruncated(t *testing.T) {
	var pre [4]byte
	binary.BigEndian.PutUint32(pre[:], uint32(DefaultMaxPacket+1))
	rd := NewReader(bytes.NewReader(pre[:]), 0)
	_, err := rd.Next()
	if got := pktErrOffset(t, err); got != 0 {
		t.Fatalf("oversize offset = %d, want 0", got)
	}

	binary.BigEndian.PutUint32(pre[:], 100)
	rd = NewReader(bytes.NewReader(append(pre[:], 1, 2, 3)), 0)
	_, err = rd.Next()
	if got := pktErrOffset(t, err); got != 7 {
		t.Fatalf("truncated payload offset = %d, want 7", got)
	}
}
