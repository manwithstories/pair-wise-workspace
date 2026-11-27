package transform

import (
	"testing"

	"relayaudit/internal/fixture"
	"relayaudit/internal/wire"
)

// TestFinalizeProducesStandaloneContainer checks that the repaired container
// carries its own header and index: scanning the output must trust the index
// rather than rebuild it.
func TestFinalizeProducesStandaloneContainer(t *testing.T) {
	seg, err := fixture.Generate(fixture.Spec{Seed: 73, Size: 1 << 19, CorruptRecord: 17})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	scan, err := wire.Scan(seg.Bytes)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	e := NewEncoder()
	ranges := BuildRanges(scan.Damage, scan.SourceSize)
	for _, rec := range scan.Records {
		if _, hit := Overlaps(rec, ranges); hit {
			e.Drop(rec.Offset)
			continue
		}
		e.Add(rec)
	}
	out, err := e.Finalize(4)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}

	h, err := wire.ParseHeader(out)
	if err != nil {
		t.Fatalf("parse output header: %v", err)
	}
	if h.Tracks != 4 || int(h.RecordCount) != e.Kept() {
		t.Fatalf("output header = %+v, kept = %d", h, e.Kept())
	}
	idx, err := wire.ParseIndex(out, h)
	if err != nil {
		t.Fatalf("parse output index: %v", err)
	}
	if err := wire.VerifyIndex(idx, out, h); err != nil {
		t.Fatalf("output index disagrees with records: %v", err)
	}
	res, err := wire.Scan(out)
	if err != nil {
		t.Fatalf("rescan output: %v", err)
	}
	if res.IndexRebuilt {
		t.Fatalf("output index was not trusted")
	}
	// Scan already omitted the unparseable record, so every scanned record
	// that does not overlap a range survives.
	wantRecords := 0
	for _, rec := range scan.Records {
		if _, hit := Overlaps(rec, ranges); !hit {
			wantRecords++
		}
	}
	if len(res.Records) != wantRecords {
		t.Fatalf("output records = %d, want %d", len(res.Records), wantRecords)
	}
	// Record payloads and sequence numbers must survive the rewrite.
	for i, rec := range res.Records {
		want := e.Records()[i]
		if rec.Seq != want.Seq || rec.Track != want.Track || string(rec.Payload) != string(want.Payload) {
			t.Fatalf("record %d changed across finalize", i)
		}
	}
}

// TestRestoreThenFinalizeMatchesSinglePass proves that resuming from a staged
// prefix produces byte-identical output to one uninterrupted pass.
func TestRestoreThenFinalizeMatchesSinglePass(t *testing.T) {
	seg, err := fixture.Generate(fixture.Spec{Seed: 41, Size: 1 << 19})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	scan, err := wire.Scan(seg.Bytes)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	straight := NewEncoder()
	for _, rec := range scan.Records {
		straight.Add(rec)
	}
	want, err := straight.Finalize(4)
	if err != nil {
		t.Fatalf("finalize straight: %v", err)
	}

	const split = 200
	first := NewEncoder()
	for _, rec := range scan.Records[:split] {
		first.Add(rec)
	}
	prefix := append([]byte(nil), first.Bytes()...)

	resumed := NewEncoder()
	if err := resumed.Restore(prefix); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, rec := range scan.Records[split:] {
		resumed.Add(rec)
	}
	got, err := resumed.Finalize(4)
	if err != nil {
		t.Fatalf("finalize resumed: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("resumed output differs from single pass (%d vs %d bytes)", len(got), len(want))
	}
}
