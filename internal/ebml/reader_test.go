package ebml

import (
	"bytes"
	"errors"
	"testing"

	"archivecheck.local/archivecheck/internal/fault"
)

func mustVint(t *testing.T, v uint64, length int) []byte {
	t.Helper()
	if v >= uint64(1)<<(7*uint(length))-1 && length < 8 {
		t.Fatalf("value %d does not fit %d-byte vint", v, length)
	}
	buf := make([]byte, length)
	x := v
	for i := length - 1; i >= 0; i-- {
		buf[i] = byte(x)
		x >>= 8
	}
	buf[0] |= byte(0x80) >> (uint(length) - 1)
	return buf
}

func idBytes(id uint32) []byte {
	switch {
	case id > 0xFFFFFF:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	case id > 0xFFFF:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	case id > 0xFF:
		return []byte{byte(id >> 8), byte(id)}
	default:
		return []byte{byte(id)}
	}
}

// element builds a known-size element.
func element(t *testing.T, id uint32, payload []byte) []byte {
	t.Helper()
	out := append([]byte{}, idBytes(id)...)
	length := 1
	for uint64(len(payload)) >= uint64(1)<<(7*uint(length))-1 && length < 8 {
		length++
	}
	out = append(out, mustVint(t, uint64(len(payload)), length)...)
	return append(out, payload...)
}

// unknownElement builds an unknown-size element header.
func unknownElement(id uint32) []byte {
	out := append([]byte{}, idBytes(id)...)
	out = append(out, 0xFF) // 1-byte unknown size
	return out
}

func newReader(b []byte) *Reader { return NewReader(bytes.NewReader(b)) }

func EncIDForTest(id uint32) []byte { return idBytes(id) }
func EncSizeForTest(size uint64) []byte {
	length := 1
	for size >= uint64(1)<<(7*uint(length))-1 && length < 8 {
		length++
	}
	buf := make([]byte, length)
	v := size
	for i := length - 1; i >= 0; i-- {
		buf[i] = byte(v)
		v >>= 8
	}
	buf[0] |= byte(0x80) >> (uint(length) - 1)
	return buf
}

func TestNextKnownSize(t *testing.T) {
	data := element(t, IDTimestampScale, []byte{0x0F, 0x42, 0x40})
	r := newReader(data)
	e, err := r.Next(int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != IDTimestampScale || e.Size != 3 || e.DataOffset != 4 || e.End != 7 {
		t.Fatalf("unexpected element: %+v", e)
	}
	v, err := r.ReadUint(e)
	if err != nil || v != 1_000_000 {
		t.Fatalf("ReadUint=%d,%v", v, err)
	}
}

func TestNextRejectsSizeBeyondLimit(t *testing.T) {
	data := element(t, IDSegment, []byte{1, 2, 3})
	// Pretend the parent ends before the declared element end.
	r := newReader(data)
	_, err := r.Next(7)
	if !errors.Is(err, ErrOversized) {
		t.Fatalf("want oversized, got %v", err)
	}
	// The reported offset is the first byte beyond the enclosing limit.
	if fault.OffsetOf(err) != 7 {
		t.Fatalf("offset=%d", fault.OffsetOf(err))
	}
}

func TestNextTruncatedHeader(t *testing.T) {
	r := newReader([]byte{0x1A, 0x45})
	_, err := r.Next(2)
	if !errors.Is(err, fault.ErrTruncated) {
		t.Fatalf("want truncated, got %v", err)
	}
}

func TestInvalidVint(t *testing.T) {
	r := newReader([]byte{0x00, 0x01, 0x02})
	_, err := r.Next(3)
	if !errors.Is(err, ErrVint) {
		t.Fatalf("want invalid vint, got %v", err)
	}
}

func TestReadUintWidths(t *testing.T) {
	for _, width := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
		payload := make([]byte, width)
		payload[width-1] = 0x7f
		data := element(t, IDTrackNumber, payload)
		r := newReader(data)
		e, err := r.Next(int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		v, err := r.ReadUint(e)
		if err != nil {
			t.Fatal(err)
		}
		if v != 0x7f {
			t.Fatalf("width %d: got %d", width, v)
		}
	}
}

func TestReadIntSignExtension(t *testing.T) {
	data := element(t, IDDiscardPadding, []byte{0xFF, 0xFE})
	r := newReader(data)
	e, err := r.Next(int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.ReadInt(e)
	if err != nil {
		t.Fatal(err)
	}
	if v != -2 {
		t.Fatalf("got %d", v)
	}
}

func TestReadSmallRefusesLargePayload(t *testing.T) {
	payload := make([]byte, 32)
	data := element(t, IDCodecPrivate, payload)
	r := newReader(data)
	e, err := r.Next(int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadSmall(e, 16); !errors.Is(err, ErrOversized) {
		t.Fatalf("want oversized, got %v", err)
	}
}

func TestStreamDoesNotBuffer(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 4096)
	data := element(t, IDCodecPrivate, payload)
	r := newReader(data)
	e, err := r.Next(int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var dest bytes.Buffer
	n, err := r.Stream(e, &dest, 8192)
	if err != nil || n != 4096 || dest.Len() != 4096 {
		t.Fatalf("stream n=%d len=%d err=%v", n, dest.Len(), err)
	}
}

func TestUnknownSizeResolutionGlobals(t *testing.T) {
	seg := unknownElement(IDSegment)
	ebml := element(t, IDEBML, []byte{1})
	data := append(seg, ebml...)
	r := newReader(data)
	e, err := r.Next(int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !e.UnknownSize || e.Size != -1 {
		t.Fatalf("expected unknown size: %+v", e)
	}
	next, err := r.Next(int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != IDEBML || next.Offset != int64(len(seg)) {
		t.Fatalf("unexpected sibling: %+v", next)
	}
	res, err := ResolveUnknown(Context{Limit: int64(len(data))}, e, next.ID, next.Offset, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if res.End != int64(len(seg)) || res.SiblingID != IDEBML {
		t.Fatalf("unexpected boundary: %+v", res)
	}
}

func TestUnknownSizeRunsToLimit(t *testing.T) {
	seg := unknownElement(IDSegment)
	tracks := element(t, IDTracks, []byte{0xAE})
	data := append(seg, tracks...)
	r := newReader(data)
	e, err := r.Next(int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(int64(len(data))); err != nil {
		t.Fatal(err)
	}
	res, err := ResolveUnknown(Context{Limit: int64(len(data))}, e, 0, int64(len(data)), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if res.End != int64(len(data)) {
		t.Fatalf("end=%d", res.End)
	}
}

func TestUnknownSizeBeyondInputUsesSourceLimit(t *testing.T) {
	data := unknownElement(IDSegment)
	r := newReader(data)
	e, err := r.Next(int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := ResolveUnknown(Context{Limit: 1 << 20}, e, 0, int64(len(data)), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if res.End != int64(len(data)) {
		t.Fatalf("end=%d want %d", res.End, len(data))
	}
}

func TestFloat64(t *testing.T) {
	v, err := Float64([]byte{0x3F, 0x80, 0x00, 0x00})
	if err != nil || v != 1 {
		t.Fatalf("v=%v err=%v", v, err)
	}
	if _, err := Float64([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error")
	}
}

func TestAllowTruncatedClampsOnlyAtSourceEnd(t *testing.T) {
	// A 1-byte-size Segment declaring 200 bytes in a 20-byte file.
	data := append(EncIDForTest(IDSegment), EncSizeForTest(200)...)
	r := newReader(data)
	r.AllowTruncated = true
	r.SetSourceSize(int64(len(data)))
	// The enclosing limit is the actual input end, as demux passes it.
	e, err := r.Next(int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !e.Truncated || e.End != int64(len(data)) {
		t.Fatalf("e=%+v", e)
	}
}

func TestAllowTruncatedRefusesInFileOverrun(t *testing.T) {
	data := append(EncIDForTest(IDSegment), EncSizeForTest(200)...)
	r := newReader(data)
	r.AllowTruncated = true
	r.SetSourceSize(4096) // file is longer than the parsed buffer
	if _, err := r.Next(int64(len(data))); !errors.Is(err, ErrBoundary) {
		t.Fatalf("got %v", err)
	}
}
