package bitstream

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/pion/rtp"

	"mediatimeline/internal/mediaerr"
)

// h264NAL builds one Annex-B NAL unit.
func h264NAL(hdr byte, payload ...byte) []byte {
	out := []byte{0, 0, 1, hdr}
	return append(out, payload...)
}

func h265NAL(t byte, payload ...byte) []byte {
	out := []byte{0, 0, 1, t << 1, 0x01}
	return append(out, payload...)
}

func av1OBU(typ byte, payload ...byte) []byte {
	hdr := (typ << 3) | 0x02 // has_size_field
	out := []byte{hdr, byte(len(payload))}
	return append(out, payload...)
}

func mustErrOffset(t *testing.T, err error, want int64) {
	t.Helper()
	me, ok := mediaerr.As(err)
	if !ok {
		t.Fatalf("error %v is not a media error", err)
	}
	if me.Offset != want {
		t.Fatalf("error offset = %d, want %d (err=%v)", me.Offset, want, me)
	}
	if !me.Recoverable {
		t.Fatalf("error not recoverable: %v", me)
	}
}

func TestScanH264OK(t *testing.T) {
	data := append(append(h264NAL(0x65, 1, 2, 3), h264NAL(0x61, 4, 5, 6)...), h264NAL(0x41, 7, 8, 9)...)
	samples, err := Scan(data, H264, "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 3 {
		t.Fatalf("got %d samples", len(samples))
	}
	if !samples[0].Keyframe || samples[1].Keyframe || samples[2].Keyframe {
		t.Fatal("keyframe flags wrong")
	}
	if samples[1].Offset != 7 || samples[1].Length != 7 {
		t.Fatalf("sample1 = %+v", samples[1])
	}
}

func TestScanH264ForbiddenBit(t *testing.T) {
	good := h264NAL(0x65, 1, 2, 3)
	bad := h264NAL(0xE5, 1, 2, 3) // forbidden bit set
	data := append(good, bad...)
	_, err := Scan(data, H264, "t")
	mustErrOffset(t, err, int64(len(good)+3)) // header byte position
}

func TestScanH264Truncated(t *testing.T) {
	data := append(h264NAL(0x65, 1, 2, 3), 0, 0, 1, 0x61) // header only, no payload
	_, err := Scan(data, H264, "t")
	mustErrOffset(t, err, 7) // start code prefix of the truncated NAL
}

func TestScanH265OK(t *testing.T) {
	data := append(h265NAL(19, 1, 2, 3), h265NAL(1, 4, 5, 6)...)
	samples, err := Scan(data, H265, "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || !samples[0].Keyframe || samples[1].Keyframe {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestScanH265LayerID(t *testing.T) {
	bad := []byte{0, 0, 1, 0x02, 0x09, 1, 2, 3} // layer id bits set
	_, err := Scan(bad, H265, "t")
	mustErrOffset(t, err, 3)
}

func TestScanAV1OK(t *testing.T) {
	data := append(av1OBU(3, 1, 2), av1OBU(6, 3, 4, 5)...)
	samples, err := Scan(data, AV1, "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || !samples[0].Keyframe || samples[1].Keyframe {
		t.Fatalf("samples = %+v", samples)
	}
	if samples[1].Offset != 4 || samples[1].Length != 5 {
		t.Fatalf("sample1 = %+v", samples[1])
	}
}

func TestScanAV1TruncatedPayload(t *testing.T) {
	data := []byte{(6 << 3) | 0x02, 10, 1, 2, 3} // declares 10, has 3
	_, err := Scan(data, AV1, "t")
	mustErrOffset(t, err, int64(len(data)))
}

func TestScanAV1TruncatedLeb(t *testing.T) {
	data := []byte{(6 << 3) | 0x02, 0x80} // leb128 continuation, then EOF
	_, err := Scan(data, AV1, "t")
	mustErrOffset(t, err, int64(len(data)))
}

func TestScanAV1ForbiddenBit(t *testing.T) {
	data := []byte{0x80 | (6 << 3) | 0x02, 1, 0}
	_, err := Scan(data, AV1, "t")
	mustErrOffset(t, err, 0)
}

func buildStream(t *testing.T) ([]byte, []Sample, *Index) {
	t.Helper()
	data := append(append(h264NAL(0x65, 1, 2, 3), h264NAL(0x61, 4, 5, 6)...), h264NAL(0x41, 7, 8, 9)...)
	samples, err := Scan(data, H264, "t")
	if err != nil {
		t.Fatal(err)
	}
	idx := BuildIndex("t", H264, 1, 1000, data, samples, []uint64{0, 33333, 66666})
	return data, samples, idx
}

func TestIndexRoundTrip(t *testing.T) {
	_, _, idx := buildStream(t)
	path := filepath.Join(t.TempDir(), "t.idx.mp4")
	if err := WriteIndexFile(path, idx); err != nil {
		t.Fatal(err)
	}
	got, err := ReadIndexFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Track != "t" || len(got.Entries) != 3 || got.Entries[1].Timestamp != 33333 {
		t.Fatalf("round trip = %+v", got)
	}
	if got.Entries[0].Digest != idx.Entries[0].Digest {
		t.Fatal("digest changed across round trip")
	}
}

func TestValidateIndexOK(t *testing.T) {
	data, samples, idx := buildStream(t)
	if err := ValidateIndex(data, samples, idx, "t"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateIndexOutOfRange(t *testing.T) {
	data, samples, idx := buildStream(t)
	idx.Entries[2].Offset = int64(len(data)) + 100
	err := ValidateIndex(data, samples, idx, "t")
	mustErrOffset(t, err, int64(len(data))+100)
}

func TestValidateIndexOutOfOrder(t *testing.T) {
	data, samples, idx := buildStream(t)
	idx.Entries[1].Offset = idx.Entries[0].Offset // descending
	err := ValidateIndex(data, samples, idx, "t")
	mustErrOffset(t, err, idx.Entries[1].Offset)
}

func TestValidateIndexDigestMismatch(t *testing.T) {
	data, samples, idx := buildStream(t)
	idx.Entries[1].Digest = idx.Entries[0].Digest // forged
	err := ValidateIndex(data, samples, idx, "t")
	mustErrOffset(t, err, idx.Entries[1].Offset)
}

func TestDepacketizeRTP(t *testing.T) {
	var cap []byte
	mk := func(seq uint16, ts uint32, payload ...byte) {
		pkt := rtp.Packet{Header: rtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: ts, SSRC: 7,
		}, Payload: payload}
		b, err := pkt.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		var rec [2]byte
		binary.BigEndian.PutUint16(rec[:], uint16(len(b)))
		cap = append(cap, rec[:]...)
		cap = append(cap, b...)
	}
	mk(65534, 1000, 0xAA)
	mk(65535, 2000, 0xBB)
	mk(0, 3000, 0xCC) // sequence wrap
	mk(2, 4000, 0xDD) // one packet lost before this
	es, samples, err := DepacketizeRTP(cap, "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 4 || len(samples) != 4 {
		t.Fatalf("es=%d samples=%d", len(es), len(samples))
	}
	if samples[2].RawSeq != 0 || samples[2].Offset != 2 {
		t.Fatalf("sample2 = %+v", samples[2])
	}
	if samples[3].LostBefore != 1 {
		t.Fatalf("lost = %d, want 1", samples[3].LostBefore)
	}
	if samples[0].RawTS != 1000 || samples[3].RawTS != 4000 {
		t.Fatalf("timestamps = %+v", samples)
	}
}

func TestDepacketizeRTPTruncated(t *testing.T) {
	pkt := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 1, Timestamp: 1, SSRC: 7}, Payload: []byte{1, 2, 3}}
	b, err := pkt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var rec [2]byte
	binary.BigEndian.PutUint16(rec[:], uint16(len(b)))
	cap := append(rec[:], b[:len(b)-2]...) // truncated packet
	_, _, err = DepacketizeRTP(cap, "t")
	mustErrOffset(t, err, int64(len(cap)))
}

func TestReadIndexFileMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.mp4")
	if err := os.WriteFile(path, []byte("not an mp4"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIndexFile(path); err == nil {
		t.Fatal("expected error for garbage file")
	}
}
