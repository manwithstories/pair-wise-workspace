package wire

import (
	"testing"

	"relayaudit/internal/clock"
	"relayaudit/internal/fault"
)

// buildSegment writes a valid segment of n records plus index pages.
func buildSegment(t *testing.T, n int) ([]byte, Header, []*Record) {
	t.Helper()
	pages := PagesFor(n)
	dataStart := int64(HeaderSize) + int64(pages)*PageSize
	var recs []*Record
	var payloads [][]byte
	for i := 0; i < n; i++ {
		body := make([]byte, 32+i%17)
		for j := range body {
			body[j] = byte(i*7 + j)
		}
		recs = append(recs, mkRecord(uint8(1+i%4), uint32(i), clock.Stamp(1000+i*10), body))
		payloads = append(payloads, body)
	}
	entries := make([]Entry, 0, n)
	off := dataStart
	var stream []byte
	for _, r := range recs {
		b := r.Marshal(nil)
		entries = append(entries, Entry{Offset: off, Length: uint32(len(b)), Track: r.Track, SeqLow: uint16(r.Seq)})
		stream = append(stream, b...)
		off += int64(len(b))
	}
	h := Header{Version: Version, Tracks: 4, RecordCount: uint32(n), PageCount: uint32(pages), DataStart: dataStart}
	hb := EncodeHeader(h)
	buf := make([]byte, dataStart+int64(len(stream)))
	copy(buf[:HeaderSize], hb[:])
	copy(buf[HeaderSize:dataStart], EncodeIndex(entries))
	copy(buf[dataStart:], stream)
	for i := range recs {
		recs[i].Offset = entries[i].Offset
	}
	return buf, h, recs
}

func TestHeaderRoundTrip(t *testing.T) {
	h := Header{Version: Version, Tracks: 4, RecordCount: 100, PageCount: 2, DataStart: 24 + 2*PageSize}
	b := EncodeHeader(h)
	got, err := ParseHeader(b[:])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != h {
		t.Fatalf("header = %+v, want %+v", got, h)
	}
	if err := h.ValidateBounds(h.DataStart + 1000); err != nil {
		t.Fatalf("bounds: %v", err)
	}
}

func TestHeaderRejectsImplausibleTrackCount(t *testing.T) {
	h := Header{Version: Version, Tracks: 0, DataStart: HeaderSize}
	b := EncodeHeader(h)
	_, err := ParseHeader(b[:])
	if !IsFaultKind(err, fault.Limit) {
		t.Fatalf("err = %v, want limit", err)
	}
}

func TestHeaderRejectsDataStartBeyondFile(t *testing.T) {
	h := Header{Version: Version, Tracks: 4, PageCount: 1, DataStart: 24 + PageSize}
	if err := h.ValidateBounds(100); !IsFaultKind(err, fault.Index) {
		t.Fatalf("err = %v, want index", err)
	}
	if err := h.ValidateBounds(h.DataStart); err != nil {
		t.Fatalf("valid data start rejected: %v", err)
	}
	// A short index region is not a header error; ParseIndex reports it.
	short := Header{Version: Version, Tracks: 4, PageCount: 4, DataStart: 24 + 2*PageSize}
	if err := short.ValidateBounds(24 + 2*PageSize); err != nil {
		t.Fatalf("short index region rejected at header level: %v", err)
	}
}

func TestParseIndexAcceptsConsistentSegment(t *testing.T) {
	buf, h, _ := buildSegment(t, 130)
	idx, err := ParseIndex(buf, h)
	if err != nil {
		t.Fatalf("parse index: %v", err)
	}
	if len(idx.Entries) != 130 {
		t.Fatalf("entries = %d, want 130", len(idx.Entries))
	}
	if idx.Pages != 3 {
		t.Fatalf("pages = %d, want 3", idx.Pages)
	}
	if err := VerifyIndex(idx, buf, h); err != nil {
		t.Fatalf("verify index: %v", err)
	}
}

func TestParseIndexDetectsDroppedPage(t *testing.T) {
	buf, h, _ := buildSegment(t, 130)
	// Blank page 2; its magic disappears.
	pageOff := int64(HeaderSize) + PageSize
	clear(buf[pageOff : pageOff+PageSize])
	_, err := ParseIndex(buf, h)
	f, _ := fault.As(err)
	if f == nil || f.Kind() != fault.Index {
		t.Fatalf("err = %v, want index fault", err)
	}
	if f.Offset() != pageOff {
		t.Fatalf("offset = %d, want %d", f.Offset(), pageOff)
	}
}

func TestParseIndexDetectsTruncatedRegion(t *testing.T) {
	buf, h, _ := buildSegment(t, 130)
	short := buf[:HeaderSize+PageSize+8]
	_, err := ParseIndex(short, h)
	if !IsFaultKind(err, fault.Index) {
		t.Fatalf("err = %v, want index", err)
	}
}

func TestVerifyIndexRejectsStaleEntry(t *testing.T) {
	buf, h, _ := buildSegment(t, 20)
	idx, err := ParseIndex(buf, h)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	idx.Entries[5].Track = 3 // record 5 is on track 2, so this disagrees
	err = VerifyIndex(idx, buf, h)
	if !IsFaultKind(err, fault.Index) {
		t.Fatalf("stale index accepted: %v", err)
	}
}

func TestVerifyIndexRejectsCountMismatch(t *testing.T) {
	buf, h, _ := buildSegment(t, 20)
	idx, _ := ParseIndex(buf, h)
	idx.Entries = idx.Entries[:10]
	if err := VerifyIndex(idx, buf, h); !IsFaultKind(err, fault.Index) {
		t.Fatalf("count mismatch accepted: %v", err)
	}
}

func TestIndexFind(t *testing.T) {
	idx := &Index{Entries: []Entry{
		{Offset: 100, Length: 50},
		{Offset: 150, Length: 25},
		{Offset: 175, Length: 100},
	}}
	if e, ok := idx.Find(120); !ok || e.Offset != 100 {
		t.Fatalf("Find(120) = %+v %v", e, ok)
	}
	if e, ok := idx.Find(175); !ok || e.Offset != 175 {
		t.Fatalf("Find(175) = %+v %v", e, ok)
	}
	if e, ok := idx.Find(160); !ok || e.Offset != 150 {
		t.Fatalf("Find(160) = %+v %v, want entry at 150", e, ok)
	}
	if _, ok := idx.Find(99); ok {
		t.Fatalf("Find(99) before the first entry")
	}
	if _, ok := idx.Find(275); ok {
		t.Fatalf("Find(275) at the exclusive end")
	}
	if _, ok := idx.Find(500); ok {
		t.Fatalf("Find(500) out of range")
	}
}

func TestPagesFor(t *testing.T) {
	cases := map[int]int{0: 0, 1: 1, 64: 1, 65: 2, 128: 2, 129: 3}
	for n, want := range cases {
		if got := PagesFor(n); got != want {
			t.Fatalf("PagesFor(%d) = %d, want %d", n, got, want)
		}
	}
}
