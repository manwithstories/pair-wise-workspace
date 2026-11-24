package ingest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/pion/rtp"

	"github.com/clearliu/mediq/internal/core"
)

func frameDatagram(t *testing.T, d []byte) []byte {
	t.Helper()
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(d)))
	return append(prefix[:], d...)
}

func marshalRTP(t *testing.T, seq uint16, ts uint32) []byte {
	t.Helper()
	pkt := rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: ts, SSRC: 42},
		Payload: []byte{1, 2, 3, 4},
	}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestReaderReadsRTPAndSenderReport(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(frameDatagram(t, marshalRTP(t, 7, 700)))
	sr := make([]byte, 28)
	sr[0] = 0x80
	sr[1] = 200
	binary.BigEndian.PutUint16(sr[2:4], 6)
	binary.BigEndian.PutUint32(sr[4:8], 42)
	binary.BigEndian.PutUint32(sr[16:20], 700)
	stream.Write(frameDatagram(t, sr))

	r := NewReader(bytes.NewReader(stream.Bytes()), 1024)
	rec, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Kind != KindRTP || rec.Packet.SequenceNumber != 7 || rec.Offset != 4 {
		t.Fatalf("bad RTP record: %+v", rec)
	}
	rec, err = r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Kind != KindRTCP || rec.SR == nil || rec.SR.SSRC != 42 || rec.SR.RTPTimestamp != 700 {
		t.Fatalf("bad SR record: %+v", rec)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("final Next error = %v, want EOF", err)
	}
}

func TestValidateRTPreportsCSRCLengthOffset(t *testing.T) {
	// V=2, CC=15: fixed header claims 72 bytes but datagram has only 20.
	bad := make([]byte, 20)
	bad[0] = 0x8f
	bad[1] = 96
	err := ValidateRTP(bad, 100)
	var ce *core.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %T is not core.Error", err)
	}
	// CSRC area begins at payload byte 12 => absolute 112.
	if ce.Offset != 112 {
		t.Fatalf("offset = %d, want 112", ce.Offset)
	}
}

func TestReaderReportsTruncatedExtensionHeaderOffset(t *testing.T) {
	bad := make([]byte, 14)
	bad[0] = 0x90 // V=2, X=1, CC=0
	bad[1] = 96
	var stream bytes.Buffer
	stream.Write(frameDatagram(t, bad))

	_, err := NewReader(bytes.NewReader(stream.Bytes()), 1024).Next()
	var ce *core.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not core.Error", err)
	}
	// Payload starts after 4-byte prefix; extension starts at payload+12.
	if ce.Offset != 16 {
		t.Fatalf("offset = %d, want 16", ce.Offset)
	}
}

func TestValidateRTPreportsBadExtensionLengthField(t *testing.T) {
	bad := make([]byte, 20)
	bad[0] = 0x90
	bad[1] = 96
	binary.BigEndian.PutUint16(bad[14:16], 10) // claims 40 extension bytes
	err := ValidateRTP(bad, 4)
	var ce *core.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not core.Error", err)
	}
	// Payload base 4 + extension byte 12 + length field byte 2 = 18.
	if ce.Offset != 18 {
		t.Fatalf("offset = %d, want 18", ce.Offset)
	}
}

func TestReaderRejectsOversizedDatagramWithoutAllocation(t *testing.T) {
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], HardMaxDatagramBytes+1)
	r := NewReader(bytes.NewReader(prefix[:]), DefaultMaxDatagramBytes)
	_, err := r.Next()
	if core.KindOf(err) != core.KindParse {
		t.Fatalf("err=%v kind=%v, want parse", err, core.KindOf(err))
	}
}
