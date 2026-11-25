package ebml

import (
	"bytes"
	"encoding/binary"
	"testing"

	"mediapipe/internal/media"
)

func encodeSize(n int64) []byte {
	// Minimal 8-byte encoding keeps fixture construction simple.
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(n))
	buf[0] |= 0x01
	return buf
}

func TestReadVintIDs(t *testing.T) {
	cases := []struct {
		raw  []byte
		want ID
	}{
		{[]byte{0x1A, 0x45, 0xDF, 0xA3}, IDEBML},
		{[]byte{0xA3}, IDSimpleBlock},
		{[]byte{0x1F, 0x43, 0xB6, 0x75}, IDCluster},
	}
	for _, c := range cases {
		r := NewReader(bytes.NewReader(c.raw))
		got, n, err := r.ReadID()
		if err != nil {
			t.Fatalf("ReadID(%x): %v", c.raw, err)
		}
		if got != c.want {
			t.Fatalf("ReadID(%x)=%#x want %#x", c.raw, uint32(got), uint32(c.want))
		}
		if n != len(c.raw) {
			t.Fatalf("ReadID length=%d want %d", n, len(c.raw))
		}
	}
}

func TestReadSizeKnownAndUnknown(t *testing.T) {
	r := NewReader(bytes.NewReader(encodeSize(1234)))
	size, unknown, n, err := r.ReadSize()
	if err != nil || unknown || size != 1234 || n != 8 {
		t.Fatalf("size=%d unknown=%v n=%d err=%v", size, unknown, n, err)
	}

	r = NewReader(bytes.NewReader([]byte{0xFF}))
	size, unknown, n, err = r.ReadSize()
	if err != nil || !unknown || size != SizeUnknown || n != 1 {
		t.Fatalf("unknown size handling: size=%d unknown=%v n=%d err=%v", size, unknown, n, err)
	}
}

func TestReadSizeAllOnesThreeBytes(t *testing.T) {
	// 0x3F is a 3-byte size VINT whose payload bits are all ones.
	r := NewReader(bytes.NewReader([]byte{0x3F, 0xFF, 0xFF}))
	size, unknown, _, err := r.ReadSize()
	if err != nil || !unknown || size != SizeUnknown {
		t.Fatalf("size=%d unknown=%v err=%v", size, unknown, err)
	}
}

func TestVintOverflowRejected(t *testing.T) {
	// EBML IDs longer than 4 bytes are illegal.
	r := NewReader(bytes.NewReader([]byte{0x08, 0x00, 0x00, 0x00, 0x01}))
	_, _, err := r.ReadID()
	if err == nil {
		t.Fatal("expected overflow error for 5-byte ID")
	}
	if media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("class=%v", media.ClassOf(err))
	}
	if off, ok := media.OffsetOf(err); !ok || off != 0 {
		t.Fatalf("offset=%d ok=%v", off, ok)
	}
}

func TestVintZeroByteRejected(t *testing.T) {
	r := NewReader(bytes.NewReader([]byte{0x00, 0x01, 0x02}))
	_, _, err := r.ReadID()
	if err == nil {
		t.Fatal("expected leading-zero error")
	}
}

func TestUnknownSizeElementRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0x18, 0x53, 0x80, 0x67}) // Segment
	buf.WriteByte(0xFF)                      // unknown size
	buf.Write([]byte{0x1F, 0x43, 0xB6, 0x75}) // Cluster
	buf.WriteByte(0xFF)                      // unknown size
	buf.Write([]byte{0xE7})
	buf.Write(encodeSize(8))
	buf.Write([]byte{0, 0, 0, 0, 0, 0, 0, 1})

	r := NewReader(bytes.NewReader(buf.Bytes()))
	seg, err := r.ReadElement()
	if err != nil || !seg.Unknown || seg.ID != IDSegment {
		t.Fatalf("segment: %+v err=%v", seg, err)
	}
	cl, err := r.ReadElement()
	if err != nil || !cl.Unknown || cl.ID != IDCluster {
		t.Fatalf("cluster: %+v err=%v", cl, err)
	}
	if cl.Offset != 5 {
		t.Fatalf("cluster offset=%d want 5", cl.Offset)
	}
	timestamp, err := r.ReadElement()
	if err != nil || timestamp.ID != IDTimestamp || timestamp.Payload != 8 {
		t.Fatalf("timestamp: %+v err=%v", timestamp, err)
	}
}

func TestTruncatedPayloadReportsOffset(t *testing.T) {
	// Element claims 8 payload bytes but supplies 2.
	var buf bytes.Buffer
	buf.Write([]byte{0xE7})
	buf.Write(encodeSize(8))
	buf.Write([]byte{0x01, 0x02})
	_ = buf

	raw := append([]byte{0xE7}, encodeSize(8)...)
	raw = append(raw, 0x01, 0x02)
	r := NewReader(bytes.NewReader(raw))
	el, err := r.ReadElement()
	if err != nil {
		t.Fatalf("ReadElement: %v", err)
	}
	if _, err := r.ReadUint(int(el.Payload)); err == nil {
		t.Fatal("expected truncation error")
	} else if off, ok := media.OffsetOf(err); !ok || off != int64(len(raw)) {
		t.Fatalf("truncation offset=%d ok=%v", off, ok)
	}
}

func TestElementSizeLimit(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxElementSize = 16
	r := NewReaderWithLimits(bytes.NewReader([]byte{0xA3}), lim)
	_ = r
	raw := append([]byte{0xA3}, encodeSize(1024)...)
	r = NewReaderWithLimits(bytes.NewReader(raw), lim)
	_, err := r.ReadElement()
	if err == nil || media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("expected fatal element-size error, got %v", err)
	}
}

func TestReadUintSignExtend(t *testing.T) {
	r := NewReader(bytes.NewReader([]byte{0xFF, 0xFE}))
	v, err := r.ReadInt(2)
	if err != nil || v != -2 {
		t.Fatalf("v=%d err=%v", v, err)
	}
}

func TestReadFloat(t *testing.T) {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, 0x4059000000000000)
	r := NewReader(bytes.NewReader(buf))
	f, err := r.ReadFloat(8)
	if err != nil || f != 100 {
		t.Fatalf("f=%v err=%v", f, err)
	}
}

func TestOffsetTracksAcrossElement(t *testing.T) {
	raw := []byte{0x1A, 0x45, 0xDF, 0xA3}
	raw = append(raw, encodeSize(0)...)
	r := NewReader(bytes.NewReader(raw))
	if _, err := r.ReadElement(); err != nil {
		t.Fatal(err)
	}
	if r.Offset() != int64(len(raw)) {
		t.Fatalf("offset=%d want %d", r.Offset(), len(raw))
	}
}
