package wire

import (
	"testing"

	"relayaudit/internal/fault"
)

func TestRebuildRecoversCleanRecords(t *testing.T) {
	buf, h, want := buildSegment(t, 257)
	idx, recs, damage, err := Rebuild(buf, h, int64(len(buf)))
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if len(damage) != 0 {
		t.Fatalf("clean input reported damage: %v", damage[0])
	}
	if len(idx.Entries) != len(want) || len(recs) != len(want) {
		t.Fatalf("rebuilt %d/%d records, want %d", len(idx.Entries), len(recs), len(want))
	}
	for i := range want {
		if recs[i].Rec.Offset != want[i].Offset || recs[i].Rec.Seq != want[i].Seq {
			t.Fatalf("record %d mismatch: %+v vs %+v", i, recs[i].Rec, want[i])
		}
	}
	if !idx.Rebuilt {
		t.Fatalf("Rebuilt flag not set")
	}
	if idx.Pages != PagesFor(len(want)) {
		t.Fatalf("pages = %d", idx.Pages)
	}
}

func TestRebuildSkipsCorruptRecordWithOriginalOffset(t *testing.T) {
	buf, h, want := buildSegment(t, 64)
	bad := want[17]
	buf[bad.Offset+RecordHeaderSize+2] ^= 0xff
	idx, recs, damage, err := Rebuild(buf, h, int64(len(buf)))
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if len(damage) != 1 {
		t.Fatalf("damage count = %d, want 1", len(damage))
	}
	if damage[0].Kind() != fault.CRC || damage[0].Offset() != bad.Offset {
		t.Fatalf("damage = %v at %d, want crc at %d", damage[0].Kind(), damage[0].Offset(), bad.Offset)
	}
	if len(idx.Entries) != 63 {
		t.Fatalf("rebuilt %d entries, want 63", len(idx.Entries))
	}
	if recs[17].Rec.Seq != want[18].Seq {
		t.Fatalf("resync did not skip to next record")
	}
}

func TestRebuildStopsAtTruncatedTail(t *testing.T) {
	buf, h, want := buildSegment(t, 40)
	cut := buf[:len(buf)-7]
	idx, _, damage, _ := Rebuild(cut, h, int64(len(cut)))
	if len(idx.Entries) != 39 {
		t.Fatalf("entries = %d, want 39", len(idx.Entries))
	}
	if len(damage) != 1 || damage[0].Kind() != fault.Truncated {
		t.Fatalf("damage = %v, want one truncated fault", damage)
	}
	lastGood := want[38]
	if damage[0].Offset() != lastGood.Offset+lastGood.Size() {
		t.Fatalf("truncation offset = %d, want %d", damage[0].Offset(), lastGood.Offset+lastGood.Size())
	}
}

func TestRebuildDoesNotReadPastEnd(t *testing.T) {
	buf, h, _ := buildSegment(t, 10)
	cut := buf[:h.DataStart+100]
	_, recs, _, _ := Rebuild(cut, h, int64(len(cut)))
	for _, r := range recs {
		if r.Rec.End() > int64(len(cut)) {
			t.Fatalf("record read past end: %d > %d", r.Rec.End(), len(cut))
		}
	}
}

