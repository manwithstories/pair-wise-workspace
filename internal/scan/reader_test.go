package scan

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"medpipe/internal/model"
)

// buildContainer writes n video records and n/2 audio records with
// deterministic, checksum-correct payloads.
func buildContainer(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	count := uint32(n + n/2)
	if err := model.WriteHeader(&buf, count, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		meta := []byte(`{"width":16,"height":16,"color":"yuv420","keyframe":` +
			boolStr(i%30 == 0) + `,"frame_num":` + itoa(uint32(i)) + `}`)
		payload := make([]byte, 64)
		binary.BigEndian.PutUint32(payload, uint32(i))
		flags := uint16(0)
		if i%30 == 0 {
			flags = model.FlagKeyframe
		}
		if _, err := model.WriteRecord(&buf, model.KindVideo, flags, uint32(i),
			int64(i)*40_000_000, meta, payload); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n/2; i++ {
		meta := []byte(`{"sample_rate":8000,"channels":1,"samples":160}`)
		payload := make([]byte, 320)
		if _, err := model.WriteRecord(&buf, model.KindAudio, 0, uint32(i),
			int64(i)*80_000_000, meta, payload); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func scanBytes(t *testing.T, raw []byte) *Result {
	t.Helper()
	rd, err := NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	res, err := rd.Scan()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return res
}

func TestScanHappyPath(t *testing.T) {
	raw := buildContainer(t, 12)
	res := scanBytes(t, raw)
	if len(res.Frames) != 12 || len(res.Audio) != 6 {
		t.Fatalf("counts frames=%d audio=%d", len(res.Frames), len(res.Audio))
	}
	if res.TailTruncated || res.FirstCorruption != nil {
		t.Fatalf("unexpected failure trunc=%v corr=%v", res.TailTruncated, res.FirstCorruption)
	}
	if res.Frames[3].Seq != 3 || res.Frames[3].Width != 16 || !res.Frames[0].Keyframe {
		t.Fatal("decoded frame fields wrong")
	}
	if res.RecoverableOffset != int64(len(raw)) {
		t.Fatalf("recoverable offset %d != size %d", res.RecoverableOffset, len(raw))
	}
}

func TestScanTailTruncation(t *testing.T) {
	raw := buildContainer(t, 10)
	// Drop the final 9 bytes: the last record's checksum is incomplete.
	cut := append([]byte(nil), raw[:len(raw)-9]...)
	res := scanBytes(t, cut)
	if !res.TailTruncated {
		t.Fatal("expected tail truncation flag")
	}
	if res.FirstCorruption != nil {
		t.Fatalf("truncation must not be reported as corruption: %v", res.FirstCorruption)
	}
	// The final record is audio; all ten video frames stay intact.
	if res.CompleteRecords != 14 || len(res.Frames) != 10 || len(res.Audio) != 4 {
		t.Fatalf("kept records=%d frames=%d audio=%d, want 14/10/4",
			res.CompleteRecords, len(res.Frames), len(res.Audio))
	}
	if res.RecoverableOffset >= int64(len(cut)) {
		t.Fatal("recoverable boundary must precede EOF")
	}
}

func TestScanMidStreamChecksumCorruption(t *testing.T) {
	raw := buildContainer(t, 10)
	// Locate the 4th video record (seq 3) size field and flip a payload byte.
	rd, _ := NewReader(bytes.NewReader(raw))
	hdr, err := rd.ReadHeader()
	if err != nil {
		t.Fatal(err)
	}
	off := hdr.Offset
	target := int64(-1)
	for i := 0; i < 4; i++ {
		size := int64(binary.BigEndian.Uint32(raw[off : off+4]))
		if i == 3 {
			target = off + size - 40 // inside payload, before checksum
		}
		off += size
	}
	raw[target] ^= 0xFF
	res := scanBytes(t, raw)
	if res.FirstCorruption == nil {
		t.Fatal("expected mid-stream corruption")
	}
	if res.FirstCorruption.Code != model.ErrChecksum || res.FirstCorruption.Seq != 3 {
		t.Fatalf("wrong corruption: %+v", res.FirstCorruption)
	}
	if res.TailTruncated {
		t.Fatal("fully-read bad record is not truncation")
	}
	if len(res.Frames) != 9 {
		t.Fatalf("corrupt frame dropped, others kept: got %d", len(res.Frames))
	}
}

func TestScanBadHeader(t *testing.T) {
	raw := buildContainer(t, 2)
	raw[0] = 'X'
	rd, err := NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Scan(); err == nil || !strings.Contains(err.Error(), string(model.ErrHeader)) {
		t.Fatalf("want header error, got %v", err)
	}
}

func TestChecksumVectorStable(t *testing.T) {
	sum := model.VerifyChecksum(model.KindVideo, 0, 1, 100, []byte("m"), []byte("p"),
		checksumOf(model.KindVideo, 0, 1, 100, []byte("m"), []byte("p")))
	if sum != nil {
		t.Fatalf("self checksum failed: %v", sum)
	}
}

func checksumOf(kind, flags uint16, seq uint32, ts int64, meta, payload []byte) [32]byte {
	// Reuse the public writer, which returns the computed digest.
	var buf bytes.Buffer
	sum, _ := model.WriteRecord(&buf, kind, flags, seq, ts, meta, payload)
	return sum
}
