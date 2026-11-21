package ebml

import (
	"bytes"
	"strings"
	"testing"

	"mkvarchive/internal/model"
)

func TestVINTRoundTrip(t *testing.T) {
	cases := []uint64{0, 1, 0x7E, 0x7F, 0x3FFF, 0x1FFFFF, 0x0FFFFFFF, 0x100000000, 1 << 40}
	for _, v := range cases {
		enc, err := EncodeVINT(v)
		if err != nil {
			t.Fatalf("encode %d: %v", v, err)
		}
		got, err := ReadVINT(bytes.NewReader(enc), 10)
		if err != nil {
			t.Fatalf("decode %d: %v", v, err)
		}
		if got.Value != v {
			t.Fatalf("value mismatch: got %d want %d", got.Value, v)
		}
		if got.Width != len(enc) {
			t.Fatalf("width mismatch: got %d want %d", got.Width, len(enc))
		}
	}
}

func TestUnknownSizeEncoding(t *testing.T) {
	for width := 1; width <= 8; width++ {
		enc, err := EncodeVINTWidth(UnknownSizeValue(width), width)
		if err != nil {
			t.Fatalf("width %d: %v", width, err)
		}
		got, err := ReadVINT(bytes.NewReader(enc), 0)
		if err != nil {
			t.Fatalf("width %d decode: %v", width, err)
		}
		if got.Value != UnknownSizeValue(width) {
			t.Fatalf("width %d value mismatch", width)
		}
	}
}

func TestTruncatedHeaderOffset(t *testing.T) {
	// One complete element (id 0x81, size 1, payload), then a 4-byte VINT
	// marker byte whose three remaining bytes are missing.
	var data bytes.Buffer
	hdr, _ := EncodeID(0x81)
	size, _ := EncodeVINT(1)
	data.Write(hdr)
	data.Write(size)
	data.WriteByte(0x42)
	data.WriteByte(0x10) // 4-byte VINT marker, no payload follows
	r := NewReader(bytes.NewReader(data.Bytes()))
	e, err := r.Next()
	if err != nil {
		t.Fatalf("first element: %v", err)
	}
	if e.ID != 0x81 || e.Size != 1 || e.HeaderOff != 0 {
		t.Fatalf("unexpected element %+v", e)
	}
	if _, err = r.ReadData(e); err != nil {
		t.Fatalf("payload: %v", err)
	}
	_, err = r.Next()
	ae, ok := err.(*model.ArchiveError)
	if !ok || ae.Code != "EBML_TRUNCATED_HEADER" || ae.Offset != 3 {
		t.Fatalf("want anchored truncation at 3, got %v", err)
	}
}

func TestOverrunReportsHeaderOffset(t *testing.T) {
	// 4 bytes of unrelated prefix, then an element header at offset 4 that
	// declares 100 payload bytes while only 3 are present.
	data := bytes.NewBuffer([]byte{0xAA, 0xBB, 0xCC, 0xDD})
	hdr, _ := EncodeID(0xA3)
	sz, _ := EncodeVINT(100)
	data.Write(hdr)
	data.Write(sz)
	data.Write([]byte{1, 2, 3})
	raw := data.Bytes()
	r := NewReader(bytes.NewReader(raw[4:]))
	r.offset = 4
	e, err := r.Next()
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	if e.HeaderOff != 4 || e.DataOff != 6 {
		t.Fatalf("unexpected offsets: %+v", e)
	}
	if _, err = r.ReadData(e); err == nil || !strings.Contains(err.Error(), "offset=4") {
		t.Fatalf("want overrun anchored at header offset 4, got %v", err)
	}
}

func TestEncodeIDWidths(t *testing.T) {
	cases := []struct {
		id    uint32
		width int
	}{
		{0xA3, 1}, {0xAE, 1}, {0x73C5, 2}, {0x1A45DFA3, 4}, {0x18538067, 4},
	}
	for _, c := range cases {
		enc, err := EncodeID(c.id)
		if err != nil || len(enc) != c.width {
			t.Fatalf("id %x: enc=% x err=%v want width %d", c.id, enc, err, c.width)
		}
		got, err := ReadElementID(bytes.NewReader(enc), 0)
		if err != nil || got.Value != uint64(c.id) {
			t.Fatalf("id %x round trip: %+v %v", c.id, got, err)
		}
	}
	if _, err := EncodeID(0x0102); err == nil {
		t.Fatal("0x0102 has no marker in leading byte 0x01, must be rejected")
	}
}

func TestUnknownLengthMasterAndChildren(t *testing.T) {
	// Master ID 0x18538067 with unknown 8-byte size, one child 0x81=0x7F.
	var buf bytes.Buffer
	id, _ := EncodeID(0x18538067)
	size, _ := EncodeVINTWidth(UnknownSizeValue(8), 8)
	childID, _ := EncodeID(0x81)
	childSize, _ := EncodeVINT(1)
	buf.Write(id)
	buf.Write(size)
	buf.Write(childID)
	buf.Write(childSize)
	buf.WriteByte(0x7F)

	r := NewReader(bytes.NewReader(buf.Bytes()))
	master, err := r.Next()
	if err != nil {
		t.Fatalf("master header: %v", err)
	}
	if master.Size != UnknownSize || master.DataOff != 12 {
		t.Fatalf("unexpected master: %+v", master)
	}
	cr, closer, err := r.ChildReader(master, -1)
	if err != nil {
		t.Fatalf("child reader: %v", err)
	}
	child, err := cr.Next()
	if err != nil {
		t.Fatalf("child header: %v", err)
	}
	payload, err := cr.ReadData(child)
	if err != nil || payload[0] != 0x7F {
		t.Fatalf("child payload: %v %v", payload, err)
	}
	if err = closer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if r.Offset() != 15 {
		t.Fatalf("parent offset not restored: %d", r.Offset())
	}
}
