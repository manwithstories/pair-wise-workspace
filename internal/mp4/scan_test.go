package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"mediad/internal/media"
)

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(len(out)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func fullBox(typ string, version byte, flags [3]byte, payload []byte) []byte {
	p := make([]byte, 4+len(payload))
	p[0] = version
	copy(p[1:4], flags[:])
	copy(p[4:], payload)
	return box(typ, p)
}

func TestScanValidNestedBoundaries(t *testing.T) {
	trak := box("trak", box("mdia", box("minf", box("stbl", nil))))
	moov := box("moov", trak)
	data := append(box("ftyp", []byte("isom")), moov...)
	data = append(data, box("mdat", make([]byte, 16))...)

	boxes, err := Scan(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 3 || boxes[0].Type != "ftyp" || boxes[1].Type != "moov" || boxes[2].Type != "mdat" {
		t.Fatalf("top boxes = %#v", boxes)
	}
	stbl := boxes[1].Find("trak", "mdia", "minf", "stbl")
	if stbl == nil || stbl.Offset != 44 {
		t.Fatalf("stbl not found at expected boundary: %#v", stbl)
	}
	if got := boxes[2].Size; got != 24 {
		t.Fatalf("mdat size = %d, want 24", got)
	}
}

func TestScanRejectsSizeSmallerThanHeaderAndLocatesByte(t *testing.T) {
	data := box("ftyp", nil)
	binary.BigEndian.PutUint32(data[0:4], 4)
	_, err := Scan(bytes.NewReader(data), int64(len(data)))
	var f *media.Fault
	if !errors.As(err, &f) || f.Code != media.CodeBoxBounds {
		t.Fatalf("err=%v, want box bounds fault", err)
	}
	if f.ByteRange == nil || f.ByteRange.Offset != 0 || f.ByteRange.Length != 4 {
		t.Fatalf("byte range = %#v", f.ByteRange)
	}
}

func TestScanRejectsOutOfBoundsSizeWithHeaderOffset(t *testing.T) {
	data := box("moov", nil)
	binary.BigEndian.PutUint32(data[0:4], 4096)
	_, err := Scan(bytes.NewReader(data), int64(len(data)))
	var f *media.Fault
	if !errors.As(err, &f) {
		t.Fatalf("expected structured fault, got %v", err)
	}
	if f.Code != media.CodeBoxBounds || f.ByteRange == nil || f.ByteRange.Offset != 0 {
		t.Fatalf("unexpected fault: %#v", f)
	}
}

func TestScanSalvageClampsTruncatedMDAT(t *testing.T) {
	data := box("ftyp", []byte("isom"))
	mdatOffset := len(data)
	mdat := box("mdat", make([]byte, 32))
	binary.BigEndian.PutUint32(mdat[:4], 1024) // declared far beyond actual file
	data = append(data, mdat...)
	data = data[:len(data)-20] // truncate actual payload/file

	boxes, faults, err := ScanSalvage(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(faults) != 1 || faults[0].Code != media.CodeTruncatedMDAT {
		t.Fatalf("faults=%#v", faults)
	}
	if faults[0].ByteRange == nil || faults[0].ByteRange.Offset != int64(mdatOffset) {
		t.Fatalf("fault location = %#v, want mdat offset %d", faults[0].ByteRange, mdatOffset)
	}
	if len(boxes) != 2 || boxes[1].Type != "mdat" || boxes[1].End() != int64(len(data)) {
		t.Fatalf("mdat was not clamped to EOF: %#v", boxes)
	}
}

func TestScanRejectsTruncatedHeader(t *testing.T) {
	data := []byte{0, 0, 0, 16, 'm', 'o', 'o'}
	_, err := Scan(bytes.NewReader(data), int64(len(data)))
	var f *media.Fault
	if !errors.As(err, &f) || f.ByteRange == nil || f.ByteRange.Offset != 0 {
		t.Fatalf("expected header fault at offset zero, got %v", err)
	}
}

func TestScanRejectsFileOverLimit(t *testing.T) {
	_, err := Scan(bytes.NewReader(nil), MaxFileSize+1)
	var f *media.Fault
	if !errors.As(err, &f) || f.Code != media.CodeFileTooLarge {
		t.Fatalf("err=%v", err)
	}
}

func TestFullBoxHelperBuildsVersionByte(t *testing.T) {
	b := fullBox("stts", 1, [3]byte{0, 0, 1}, []byte{1, 2})
	if b[8] != 1 || b[11] != 1 {
		t.Fatalf("fullbox header malformed: % x", b[8:12])
	}
}
