package frame

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mediamend/mediamend/internal/fault"
)

func encodeStream(t *testing.T, n int) ([]byte, []Header, [][]byte) {
	t.Helper()
	var buf []byte
	var hdrs []Header
	var payloads [][]byte
	for i := 0; i < n; i++ {
		h := Header{
			Camera: "cam1", Seq: uint64(i), DeviceTS: uint64(i) * 3000, Rate: 90000,
		}
		if i%5 == 0 {
			h.Flags = FlagKeyframe
		}
		payload := bytes.Repeat([]byte{byte(i)}, 16+i)
		buf = Encode(buf, h, payload)
		hdrs = append(hdrs, h)
		payloads = append(payloads, payload)
	}
	return buf, hdrs, payloads
}

func TestEncodeReadRoundTrip(t *testing.T) {
	buf, hdrs, payloads := encodeStream(t, 25)
	r := NewReader(bytes.NewReader(buf), "cam1", 0)
	for i := 0; i < 25; i++ {
		rec, err := r.Next()
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if rec.Camera != hdrs[i].Camera || rec.Seq != hdrs[i].Seq || rec.DeviceTS != hdrs[i].DeviceTS {
			t.Fatalf("record %d header mismatch: %+v", i, rec.Header)
		}
		if !bytes.Equal(rec.Payload, payloads[i]) {
			t.Fatalf("record %d payload mismatch", i)
		}
		if rec.Keyframe() != (i%5 == 0) {
			t.Fatalf("record %d keyframe=%v", i, rec.Keyframe())
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
	if len(r.Faults()) != 0 {
		t.Fatalf("unexpected faults: %v", r.Faults())
	}
}

func TestTruncatedRecordReportsOffset(t *testing.T) {
	buf, _, _ := encodeStream(t, 3)
	// Cut inside the third record's body.
	full := buf[:len(buf)-10]
	// Compute where the third record starts by decoding the first two.
	r0 := NewReader(bytes.NewReader(buf), "cam1", 0)
	var thirdOff int64
	for i := 0; i < 3; i++ {
		rec, err := r0.Next()
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			thirdOff = rec.Offset
		}
	}
	r := NewReader(bytes.NewReader(full), "cam1", 0)
	for {
		_, err := r.Next()
		if err != nil {
			if !errors.Is(err, ErrBadFraming) {
				t.Fatalf("want ErrBadFraming, got %v", err)
			}
			break
		}
	}
	f, ok := r.Faults().FirstOf(fault.KindTruncatedRecord)
	if !ok {
		t.Fatalf("no truncated_record fault: %v", r.Faults())
	}
	if f.Offset != thirdOff {
		t.Fatalf("truncated offset=%d want %d", f.Offset, thirdOff)
	}
	if !f.Fatal {
		t.Fatal("truncation must be fatal for the stream")
	}
}

func TestUnknownVersion(t *testing.T) {
	buf, _, _ := encodeStream(t, 1)
	buf[4] = 9
	r := NewReader(bytes.NewReader(buf), "cam1", 0)
	if _, err := r.Next(); !errors.Is(err, ErrBadFraming) {
		t.Fatalf("want framing error, got %v", err)
	}
	f, ok := r.Faults().FirstOf(fault.KindUnknownVersion)
	if !ok || f.Offset != 0 {
		t.Fatalf("version fault=%+v ok=%v", f, ok)
	}
}

func TestLengthOutOfBounds(t *testing.T) {
	buf, _, _ := encodeStream(t, 2)
	// Split: record 0 stays valid, record 1's length is huge.
	r0 := NewReader(bytes.NewReader(buf), "cam1", 0)
	_, _ = r0.Next()
	rec2, _ := r0.Next()
	off := int(rec2.Offset)
	buf[off+5] = 0xff
	buf[off+6] = 0xff
	buf[off+7] = 0xff

	r := NewReader(bytes.NewReader(buf), "cam1", 1<<16)
	if _, err := r.Next(); err != nil {
		t.Fatalf("first record: %v", err)
	}
	if _, err := r.Next(); !errors.Is(err, ErrBadFraming) {
		t.Fatalf("want framing error, got %v", err)
	}
	f, ok := r.Faults().FirstOf(fault.KindLengthOutOfBounds)
	if !ok || f.Offset != int64(off) {
		t.Fatalf("bounds fault=%+v ok=%v want offset %d", f, ok, off)
	}
}

func TestCRCFailureSkipsOnlyThatRecord(t *testing.T) {
	buf, _, payloads := encodeStream(t, 4)
	r0 := NewReader(bytes.NewReader(buf), "cam1", 0)
	var third int64
	for i := 0; i < 3; i++ {
		rec, _ := r0.Next()
		if i == 2 {
			third = rec.Offset
		}
	}
	buf[third+headerSize+5] ^= 0xff // flip a body byte of record 2

	r := NewReader(bytes.NewReader(buf), "cam1", 0)
	var got []uint64
	for {
		rec, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream must stay in sync: %v", err)
		}
		got = append(got, rec.Seq)
	}
	if len(got) != 3 {
		t.Fatalf("records after skip=%v want [0 1 3]", got)
	}
	f, ok := r.Faults().FirstOf(fault.KindChecksumMismatch)
	if !ok || f.Offset != third || f.Fatal {
		t.Fatalf("crc fault=%+v ok=%v", f, ok)
	}
	if len(payloads) != 4 {
		t.Fatal("sanity")
	}
}

func TestMetadataWriterAndVerify(t *testing.T) {
	dir := t.TempDir()
	mw, err := CreateMetadata(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	payloads := [][]byte{[]byte("alpha"), []byte("beta"), []byte("gamma")}
	var digests []string
	for i, p := range payloads {
		d, err := mw.Write(MetaRequest{
			Camera: "cam1", Seq: uint64(i), DeviceTS: uint64(i * 40),
			Length: uint32(len(p)), Offset: int64(i * 100), Keyframe: i == 0,
		}, p)
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, d)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	md, err := LoadMetadata(filepath.Join(dir, MetadataFileName))
	if err != nil {
		t.Fatal(err)
	}
	if md.Len() != 3 {
		t.Fatalf("entries=%d", md.Len())
	}
	for i, p := range payloads {
		e, ok := md.Lookup("cam1", uint64(i))
		if !ok {
			t.Fatalf("missing entry %d", i)
		}
		sum := sha256.Sum256(p)
		if e.SHA256 != hex.EncodeToString(sum[:]) || e.SHA256 != digests[i] {
			t.Fatalf("entry %d digest=%s want %s", i, e.SHA256, digests[i])
		}
		if e.Keyframe != (i == 0) {
			t.Fatalf("entry %d keyframe=%v", i, e.Keyframe)
		}
		ok, f := VerifyPayload(e, p, "src")
		if !ok || f.Kind != "" {
			t.Fatalf("entry %d verify failed: %+v", i, f)
		}
		tampered := append([]byte(nil), p...)
		tampered[0] ^= 1
		ok, f = VerifyPayload(e, tampered, "src")
		if ok || f.Kind != fault.KindChecksumMismatch || f.Offset != e.Offset {
			t.Fatalf("tamper not detected: ok=%v fault=%+v", ok, f)
		}
	}
}

func TestMetadataMalformedLineKeepsRest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MetadataFileName)
	content := `{"camera_id":"c","seq":0,"sha256":"aa"}
not json
{"camera_id":"c","seq":1,"sha256":"bb"}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	md, err := LoadMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if md.Len() != 2 {
		t.Fatalf("entries=%d want 2", md.Len())
	}
	if md.Faults().Count(fault.KindMalformedMetadata) != 1 {
		t.Fatalf("faults=%v", md.Faults())
	}
	f := md.Faults()[0]
	if f.Offset != int64(len(`{"camera_id":"c","seq":0,"sha256":"aa"}`)+1) {
		t.Fatalf("malformed line offset=%d", f.Offset)
	}
}
