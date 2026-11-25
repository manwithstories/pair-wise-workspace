package ebml

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestEncodeSizeRoundTrip(t *testing.T) {
	for _, v := range []int64{0, 1, 126, 127, 128, 16382, 16383, 1 << 20, 1<<35 - 2} {
		b, err := EncodeSize(v)
		if err != nil {
			t.Fatalf("EncodeSize(%d): %v", v, err)
		}
		lim := DefaultLimits()
		lim.MaxElementSize = 1 << 40
		r := NewReaderWithLimits(bytes.NewReader(b), lim)
		got, unknown, n, err := r.ReadSize()
		if err != nil || unknown || got != v || n != len(b) {
			t.Fatalf("v=%d got=%d unknown=%v n=%d err=%v", v, got, unknown, n, err)
		}
	}
}

func TestEncodeSizeRejectsNegative(t *testing.T) {
	if _, err := EncodeSize(-1); err == nil {
		t.Fatal("expected error")
	}
}

func TestWriterContainerRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, nil)
	if err := w.StartContainerUnknown(IDSegment); err != nil {
		t.Fatal(err)
	}
	start := w.Offset()
	if err := w.Uint(IDTimestampScale, 1_000_000); err != nil {
		t.Fatal(err)
	}
	if err := w.String(IDDocType, "matroska"); err != nil {
		t.Fatal(err)
	}
	end := w.Offset()

	r := NewReader(bytes.NewReader(buf.Bytes()))
	el, err := r.ReadElement()
	if err != nil || el.ID != IDSegment || !el.Unknown {
		t.Fatalf("segment=%+v err=%v", el, err)
	}
	ts, err := r.ReadElement()
	if err != nil || ts.ID != IDTimestampScale {
		t.Fatalf("ts=%+v err=%v", ts, err)
	}
	v, err := r.ReadUint(int(ts.Payload))
	if err != nil || v != 1_000_000 {
		t.Fatalf("scale=%d err=%v", v, err)
	}
	doc, err := r.ReadElement()
	if err != nil || doc.ID != IDDocType {
		t.Fatalf("doc=%+v err=%v", doc, err)
	}
	s, err := r.ReadString(doc.Payload)
	if err != nil || s != "matroska" {
		t.Fatalf("doctype=%q err=%v", s, err)
	}
	if start != int64(len(EncodeID(IDSegment)))+1 {
		t.Fatalf("unexpected start offset %d", start)
	}
	if w.Offset() != end {
		t.Fatalf("offset=%d want %d", w.Offset(), end)
	}
}

func TestMinimalUintWidths(t *testing.T) {
	cases := []struct {
		v    uint64
		want []byte
	}{
		{0, []byte{0}},
		{1, []byte{1}},
		{255, []byte{0xFF}},
		{256, []byte{0x01, 0x00}},
	}
	for _, c := range cases {
		got := EncodeUint(c.v)
		if !bytes.Equal(got, c.want) {
			t.Fatalf("EncodeUint(%d)=%x want %x", c.v, got, c.want)
		}
	}
}

func TestMinimalIntRoundTrip(t *testing.T) {
	for _, v := range []int64{0, 1, -1, 127, -128, 128, -129, 1 << 40, -(1 << 40)} {
		b := EncodeInt(v)
		var padded [8]byte
		copy(padded[8-len(b):], b)
		got := int64(binary.BigEndian.Uint64(padded[:]))
		if len(b) < 8 {
			shift := uint(64 - 8*len(b))
			got = got << shift >> shift
		}
		if got != v {
			t.Fatalf("EncodeInt(%d)=%x decoded %d", v, b, got)
		}
	}
}

func TestCRC32Element(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, nil)
	if err := w.CRC32Element(0xDEADBEEF); err != nil {
		t.Fatal(err)
	}
	r := NewReader(bytes.NewReader(buf.Bytes()))
	el, err := r.ReadElement()
	if err != nil || el.ID != IDCRC32 {
		t.Fatalf("el=%+v err=%v", el, err)
	}
	v, err := r.ReadCRC32(el)
	if err != nil || v != 0xDEADBEEF {
		t.Fatalf("crc=%x err=%v", v, err)
	}
}

func TestWriterHasherTracksBytes(t *testing.T) {
	var buf bytes.Buffer
	h := newTestHash()
	w := NewWriter(&buf, h)
	if err := w.Element(IDVoid, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if h.n != int64(buf.Len()) {
		t.Fatalf("hash saw %d bytes, buffer has %d", h.n, buf.Len())
	}
}

type testHash struct {
	n int64
}

func newTestHash() *testHash { return &testHash{} }

func (h *testHash) Write(p []byte) (int, error) {
	h.n += int64(len(p))
	return len(p), nil
}

func (h *testHash) Sum(b []byte) []byte { return b }
func (h *testHash) Reset()              {}
func (h *testHash) Size() int           { return 0 }
func (h *testHash) BlockSize() int      { return 1 }
