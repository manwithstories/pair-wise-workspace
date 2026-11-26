package parser_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"mediascan/internal/media"
	"mediascan/internal/parser"
	"mediascan/internal/samplegen"
)

func memorySource(t *testing.T, data []byte) *parser.Source {
	t.Helper()
	return &parser.Source{
		ReaderAt: bytes.NewReader(data),
		Size:     int64(len(data)),
		SHA256:   parser.HashBytes(data),
		Name:     "memory.mp4",
	}
}

func boxBytes(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(payload)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func TestTruncatedBoxReportsDeepestOffset(t *testing.T) {
	// stbl declares enough room for a 40-byte zzzz child, but the file ends
	// after the child header and four payload bytes.
	data := boxBytes("stbl", append(
		[]byte{0, 0, 0, 40, 'z', 'z', 'z', 'z'},
		[]byte{1, 2, 3, 4}...,
	))
	// boxBytes recalculated the outer size from actual payload, so restore the
	// deliberately over-long parent declaration.
	binary.BigEndian.PutUint32(data[0:4], 48)

	_, faults := parser.ParseBoxes(memorySource(t, data))
	var got *media.Fault
	for i := range faults {
		if faults[i].Code == media.CodeTruncatedBox {
			got = &faults[i]
		}
	}
	if got == nil {
		t.Fatalf("expected truncated_box fault, got %+v", faults)
	}
	if got.ByteOffset != 8 {
		t.Fatalf("truncated offset = %d, want 8", got.ByteOffset)
	}
	if got.FileSHA256 == "" {
		t.Fatal("fault missing file hash")
	}
}

func TestIntegerOverflowBoxSize(t *testing.T) {
	data := []byte{0, 0, 0, 1, 't', 'e', 's', 't'}
	ext := make([]byte, 8)
	for i := range ext {
		ext[i] = 0xff
	}
	data = append(data, ext...)
	_, faults := parser.ParseBoxes(memorySource(t, data))
	if len(faults) != 1 || faults[0].Code != media.CodeIntegerOverflow {
		t.Fatalf("faults = %+v, want integer_overflow", faults)
	}
}

func TestBoxDepthLimit(t *testing.T) {
	var payload []byte
	for i := 0; i < parser.MaxBoxDepth+1; i++ {
		payload = boxBytes("moov", payload)
	}
	_, faults := parser.ParseBoxes(memorySource(t, payload))
	found := false
	for _, f := range faults {
		if f.Code == media.CodeBoxDepthExceeded {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected depth fault, got %+v", faults)
	}
}

func TestGeneratedDamageOffsetsAreExact(t *testing.T) {
	set, err := samplegen.Generate(t.TempDir(), 17)
	if err != nil {
		t.Fatal(err)
	}
	byIndex := map[int]*parser.File{}
	for _, f := range set.Files {
		src, err := parser.OpenSource(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		pf := parser.ParseFile(src)
		_ = src.Close()
		byIndex[f.Index] = pf
	}

	for _, want := range set.Regions {
		pf := byIndex[want.FileIndex]
		var match *media.Fault
		for i := range pf.Faults {
			f := &pf.Faults[i]
			if f.Code == want.Code && (want.TrackID == 0 || f.TrackID == want.TrackID) {
				if match == nil || abs64(f.ByteOffset-want.Offset) < abs64(match.ByteOffset-want.Offset) {
					match = f
				}
			}
		}
		if match == nil {
			t.Fatalf("file %d: missing %s at %d; faults=%+v", want.FileIndex, want.Code, want.Offset, pf.Faults)
		}
		if match.ByteOffset != want.Offset {
			t.Fatalf("%s offset = %d, want %d (error %d)", want.Code, match.ByteOffset, want.Offset, match.ByteOffset-want.Offset)
		}
		if match.FileSHA256 == "" || match.MediaTime.Timescale == 0 && want.Code == media.CodeTfdtRegression {
			t.Fatalf("fault lacks provenance/time: %+v", match)
		}
	}

	var zero bool
	for _, f := range byIndex[5].Faults {
		if f.Code == media.CodeZeroTimescale {
			zero = true
		}
	}
	if !zero {
		t.Fatal("expected zero timescale fault on file 5")
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
