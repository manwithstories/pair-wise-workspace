package ebml

import (
	"errors"
	"testing"
)

// --- minimal EBML writer helpers for tests ---

func vid(id uint64) []byte {
	switch {
	case id <= 0xFF:
		return []byte{byte(id)}
	case id <= 0xFFFF:
		return []byte{byte(id >> 8), byte(id)}
	case id <= 0xFFFFFF:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	default:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	}
}

func vsize(size uint64) []byte {
	for w := 1; w <= 8; w++ {
		if size < (uint64(1)<<(7*w))-1 {
			out := make([]byte, w)
			s := size
			for i := w - 1; i >= 0; i-- {
				out[i] = byte(s)
				s >>= 8
			}
			out[0] |= 1 << (8 - w)
			return out
		}
	}
	panic("too big")
}

func unknownSize(width int) []byte {
	out := make([]byte, width)
	for i := range out {
		out[i] = 0xFF
	}
	out[0] = 0xFF >> (width - 1)
	return out
}

func elem(id uint64, payload []byte) []byte {
	out := vid(id)
	out = append(out, vsize(uint64(len(payload)))...)
	return append(out, payload...)
}

func uintEl(id uint64, v uint64) []byte {
	var b [8]byte
	n := 8
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
		if v == 0 {
			n = i
			break
		}
	}
	return elem(id, b[n:])
}

func block(track uint64, ts int16, payload []byte) []byte {
	b := vsize(track)
	b = append(b, byte(uint16(ts)>>8), byte(uint16(ts)))
	b = append(b, 0x80)
	return append(b, payload...)
}

func blockGroup(track uint64, ts int16, dur uint64, payload []byte) []byte {
	inner := elem(IDBlock, block(track, ts, payload))
	inner = append(inner, uintEl(IDBlockDuration, dur)...)
	return elem(IDBlockGroup, inner)
}

func clusterUnknown(ts uint64, groups ...[]byte) []byte {
	out := vid(IDCluster)
	out = append(out, unknownSize(2)...)
	out = append(out, uintEl(IDTimestamp, ts)...)
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func clusterSized(ts uint64, groups ...[]byte) []byte {
	payload := uintEl(IDTimestamp, ts)
	for _, g := range groups {
		payload = append(payload, g...)
	}
	return elem(IDCluster, payload)
}

func segment(children ...[]byte) []byte {
	out := vid(IDSegment)
	out = append(out, unknownSize(8)...)
	for _, c := range children {
		out = append(out, c...)
	}
	return out
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// --- tests ---

func TestParseUnknownSizeSegmentAndClusters(t *testing.T) {
	info := elem(IDInfo, uintEl(IDTimeScale, 1_000_000))
	c1 := clusterUnknown(0, blockGroup(1, 0, 500, []byte{1, 2, 3}))
	c2 := clusterUnknown(500, blockGroup(2, 10, 250, []byte{4, 5}))
	c3 := clusterSized(750, blockGroup(3, 0, 1000, []byte{6}))
	data := segment(concat(info, c1, c2, c3))

	doc, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.TimeScale != 1_000_000 {
		t.Fatalf("TimeScale=%d", doc.TimeScale)
	}
	if len(doc.Blocks) != 3 {
		t.Fatalf("blocks=%d", len(doc.Blocks))
	}
	want := []struct {
		track uint64
		ts    int64
		dur   uint64
	}{
		{1, 0, 500},
		{2, 510, 250},
		{3, 750, 1000},
	}
	for i, w := range want {
		b := doc.Blocks[i]
		if b.Track != w.track || b.Timestamp != w.ts || b.Duration != w.dur {
			t.Errorf("block %d = track %d ts %d dur %d, want %+v", i, b.Track, b.Timestamp, b.Duration, w)
		}
	}
}

func TestTruncatedVINTReportsOffset(t *testing.T) {
	info := elem(IDInfo, uintEl(IDTimeScale, 1_000_000))
	// Timestamp element with a 2-byte size VINT whose second byte is missing.
	bad := concat(vid(IDTimestamp), []byte{0x40})
	data := segment(concat(info, bad))

	_, err := Parse(data)
	var ee *Error
	if !errors.As(err, &ee) {
		t.Fatalf("err=%v", err)
	}
	if ee.Code != ErrTruncatedVINT {
		t.Fatalf("code=%s", ee.Code)
	}
	wantOffset := int64(len(segment(info))) + 1 // position of the 0x40 size byte
	if ee.Offset != wantOffset {
		t.Fatalf("offset=%d want %d", ee.Offset, wantOffset)
	}
}

func TestSizeOverrunReportsOffset(t *testing.T) {
	// Timestamp element declaring 8 bytes but only 2 remain in the parent.
	payload := concat(vid(IDTimestamp), vsize(8), []byte{0x01, 0x02})
	data := segment(payload)

	_, err := Parse(data)
	var ee *Error
	if !errors.As(err, &ee) {
		t.Fatalf("err=%v", err)
	}
	if ee.Code != ErrSizeOverrun {
		t.Fatalf("code=%s", ee.Code)
	}
	wantOffset := int64(len(vid(IDSegment)) + 8)
	if ee.Offset != wantOffset {
		t.Fatalf("offset=%d want %d", ee.Offset, wantOffset)
	}
}

func TestUnknownSizeOnLeafRejected(t *testing.T) {
	payload := concat(vid(IDTimestamp), unknownSize(1), []byte{0x01})
	data := segment(payload)
	_, err := Parse(data)
	var ee *Error
	if !errors.As(err, &ee) || ee.Code != ErrBadElement {
		t.Fatalf("err=%v", err)
	}
}

func TestTruncatedBlockHeaderReportsTrack(t *testing.T) {
	// Block element whose payload ends inside the 3-byte header.
	blk := elem(IDBlock, []byte{0x81, 0x00})
	data := segment(clusterSized(0, blk))
	_, err := Parse(data)
	var ee *Error
	if !errors.As(err, &ee) {
		t.Fatalf("err=%v", err)
	}
	if ee.Code != ErrTruncatedVINT || ee.Track != 1 {
		t.Fatalf("code=%s track=%d", ee.Code, ee.Track)
	}
}
