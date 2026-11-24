package demux

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/pion/rtp"

	"relay/internal/media"
)

func sampleFrame(src uint32, seq uint64, kind media.Kind) *media.RawFrame {
	payload := []byte{byte(src), byte(seq), 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	return &media.RawFrame{
		Kind: kind,
		Meta: media.FrameMeta{
			SourceID: src, Sequence: seq, ClockTicks: seq * 3000,
			CaptureRefNS: int64(seq) * 33_333_333, TimescaleHz: 90000,
			Width: 320, Height: 240, Size: uint32(len(payload)),
			SHA256: sha256.Sum256(payload),
		},
		Payload: payload,
	}
}

func TestFSEQRoundtrip(t *testing.T) {
	frames := []*media.RawFrame{
		sampleFrame(1, 0, media.KindVideo),
		sampleFrame(1, 1, media.KindVideo),
		sampleFrame(1, 2, media.KindVideo),
	}
	var buf bytes.Buffer
	hdr := FSEQHeader{SourceID: 1, Kind: media.KindVideo, TimescaleHz: 30, Width: 320, Height: 240}
	if err := WriteFSEQ(&buf, hdr, frames); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFSEQ(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(frames) {
		t.Fatalf("frame count: got %d want %d", len(got), len(frames))
	}
	for i := range frames {
		if !bytes.Equal(got[i].Payload, frames[i].Payload) {
			t.Fatalf("frame %d payload altered", i)
		}
		if got[i].Meta.SHA256 != frames[i].Meta.SHA256 {
			t.Fatalf("frame %d hash mismatch", i)
		}
	}
}

func TestFSEQChecksumMismatch(t *testing.T) {
	frames := []*media.RawFrame{sampleFrame(1, 0, media.KindVideo)}
	var buf bytes.Buffer
	if err := WriteFSEQ(&buf, FSEQHeader{SourceID: 1, Kind: media.KindVideo, TimescaleHz: 30}, frames); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	// Flip a payload byte (header is 27 bytes, record header 28).
	raw[27+28] ^= 0xff
	_, err := ReadFSEQ(bytes.NewReader(raw))
	me := media.AsMediaError(err)
	if me == nil || me.Kind != media.ErrChecksumMismatch {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
}

func TestFSEQBadMagic(t *testing.T) {
	raw := make([]byte, 60)
	_, err := ReadFSEQ(bytes.NewReader(raw))
	if me := media.AsMediaError(err); me == nil || me.Kind != media.ErrMetadata {
		t.Fatalf("want metadata error, got %v", err)
	}
}

func TestMP4RoundtripPreservesBytes(t *testing.T) {
	frames := []*media.RawFrame{
		sampleFrame(7, 0, media.KindAudio),
		sampleFrame(7, 1, media.KindAudio),
		sampleFrame(7, 2, media.KindAudio),
	}
	var buf memBuffer
	hdr := FSEQHeader{SourceID: 7, Kind: media.KindAudio, TimescaleHz: 50}
	if err := WriteFragmentedMP4(&buf, hdr, frames); err != nil {
		t.Fatal(err)
	}
	got, tracks, err := ReadFragmentedMP4(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	ti, ok := tracks[7]
	if !ok || ti.Kind != media.KindAudio {
		t.Fatalf("track discovery wrong: %+v", tracks)
	}
	if len(got) != len(frames) {
		t.Fatalf("sample count: got %d want %d", len(got), len(frames))
	}
	for i := range frames {
		if !bytes.Equal(got[i].Payload, frames[i].Payload) {
			t.Fatalf("sample %d bytes altered by remux", i)
		}
		if got[i].Meta.SHA256 != frames[i].Meta.SHA256 {
			t.Fatalf("sample %d hash mismatch", i)
		}
	}
}

func TestMP4TrunMdatLengthMismatch(t *testing.T) {
	frames := []*media.RawFrame{sampleFrame(7, 0, media.KindVideo)}
	var buf memBuffer
	if err := WriteFragmentedMP4(&buf, FSEQHeader{SourceID: 7, Kind: media.KindVideo, TimescaleHz: 30, Width: 320, Height: 240}, frames); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	// Truncating the file breaks the last mdat/trun length relationship.
	_, _, err := ReadFragmentedMP4(bytes.NewReader(raw[:len(raw)-4]))
	if err == nil {
		t.Fatal("want error on truncated fMP4")
	}
}

func TestRTPLogRoundtrip(t *testing.T) {
	pkts := []*rtp.Packet{
		{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 0, SSRC: 42}, Payload: []byte("a")},
		{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 1, SSRC: 42}, Payload: []byte("bb")},
	}
	var buf bytes.Buffer
	hdr := RTPLogHeader{SourceID: 42, Kind: media.KindAudio, ClockRateHz: 48000}
	if err := WriteRTPLog(&buf, hdr, pkts); err != nil {
		t.Fatal(err)
	}
	got, gotHdr, err := ReadRTPLog(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if gotHdr.SourceID != 42 || len(got) != 2 {
		t.Fatalf("bad parse: %+v n=%d", gotHdr, len(got))
	}
	if !bytes.Equal(got[1].Packet.Payload, []byte("bb")) {
		t.Fatal("payload not preserved")
	}
}

func TestRTPLogBadLengthPrefix(t *testing.T) {
	pkts := []*rtp.Packet{
		{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 0}, Payload: []byte("x")},
	}
	var buf bytes.Buffer
	if err := WriteRTPLog(&buf, RTPLogHeader{SourceID: 1, Kind: media.KindVideo, ClockRateHz: 90000}, pkts); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	// Corrupt the first length prefix (immediately after the 26-byte header).
	raw[26] = 0xff
	raw[27] = 0xff
	_, _, err := ReadRTPLog(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("want error on oversized length prefix")
	}
}
