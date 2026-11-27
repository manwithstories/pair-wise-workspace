package wire

import (
	"testing"

	"relayaudit/internal/fault"
)

func TestScanTrustsConsistentIndex(t *testing.T) {
	buf, _, want := buildSegment(t, 90)
	res, err := Scan(buf)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.IndexRebuilt || res.StaleIndex {
		t.Fatalf("clean segment rebuilt=%v stale=%v", res.IndexRebuilt, res.StaleIndex)
	}
	if len(res.Records) != len(want) || len(res.Damage) != 0 {
		t.Fatalf("records=%d damage=%d", len(res.Records), len(res.Damage))
	}
	if res.SourceSize != int64(len(buf)) {
		t.Fatalf("source size = %d", res.SourceSize)
	}
}

func TestScanRebuildsWhenIndexPageIsMissing(t *testing.T) {
	buf, h, want := buildSegment(t, 130)
	clear(buf[HeaderSize+PageSize : HeaderSize+2*PageSize])
	res, err := Scan(buf)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !res.IndexRebuilt {
		t.Fatalf("missing page did not trigger rebuild")
	}
	if res.IndexFault == nil || res.IndexFault.Kind() != fault.Index {
		t.Fatalf("index fault not surfaced: %v", res.IndexFault)
	}
	if len(res.Records) != len(want) {
		t.Fatalf("records = %d, want %d", len(res.Records), len(want))
	}
	_ = h
}

func TestScanRebuildsWhenIndexIsTruncated(t *testing.T) {
	buf, h, _ := buildSegment(t, 200)
	// The header claims one more page than the region actually holds, so the
	// last page the reader needs overlaps the record data.
	h.PageCount++
	hb := EncodeHeader(h)
	copy(buf[:HeaderSize], hb[:])
	res, err := Scan(buf)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !res.IndexRebuilt {
		t.Fatalf("truncated index region not rebuilt")
	}
	if res.IndexFault == nil || res.IndexFault.Kind() != fault.Index {
		t.Fatalf("truncated index did not report an index fault: %v", res.IndexFault)
	}
	if len(res.Records) != 200 {
		t.Fatalf("records = %d, want 200", len(res.Records))
	}
}

func TestScanRefusesStaleIndexEvenIfItParses(t *testing.T) {
	buf, h, _ := buildSegment(t, 40)
	// Corrupt the on-wire track of record 3, so its index entry now lies.
	idx, err := ParseIndex(buf, h)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	off := idx.Entries[3].Offset
	buf[off] = 2
	res, err := Scan(buf)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !res.StaleIndex || !res.IndexRebuilt {
		t.Fatalf("stale index was trusted: stale=%v rebuilt=%v", res.StaleIndex, res.IndexRebuilt)
	}
	if res.IndexFault == nil {
		t.Fatalf("stale index fault not reported")
	}
}

func TestScanReportsDamageInOriginalCoordinates(t *testing.T) {
	buf, h, want := buildSegment(t, 50)
	bad := want[21]
	buf[bad.Offset+RecordHeaderSize+1] ^= 0xff
	res, err := Scan(buf)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Damage) != 1 || res.Damage[0].Offset() != bad.Offset {
		t.Fatalf("damage = %v, want crc at %d", res.Damage, bad.Offset)
	}
	_ = h
}

func TestScanRejectsBadHeader(t *testing.T) {
	if _, err := Scan([]byte("too short")); !IsFaultKind(err, fault.Truncated) {
		t.Fatalf("short input: %v", err)
	}
	buf, _, _ := buildSegment(t, 5)
	buf[5] = 0
	if _, err := Scan(buf); !IsFaultKind(err, fault.Limit) {
		t.Fatalf("zero tracks accepted: %v", err)
	}
}

func TestVerifyRecordMatchesDetectsDivergence(t *testing.T) {
	buf, _, want := buildSegment(t, 12)
	if err := VerifyRecordMatches(buf, want[4]); err != nil {
		t.Fatalf("matching record rejected: %v", err)
	}
	moved := *want[4]
	moved.Offset = want[5].Offset
	if err := VerifyRecordMatches(buf, &moved); err == nil {
		t.Fatalf("mismatched record accepted")
	}
	moved.Offset = int64(len(buf)) - 2
	if err := VerifyRecordMatches(buf, &moved); !IsFaultKind(err, fault.Truncated) {
		t.Fatalf("out-of-range record: %v", err)
	}
}
