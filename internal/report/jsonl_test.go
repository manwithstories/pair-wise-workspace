package report

import (
	"path/filepath"
	"testing"
)

func sampleRecord(c string) Record {
	return Record{
		Case: c, Representations: 3, Segments: 72,
		TargetBPS: 800000, ActualBPS: 812000, BitrateDeltaPct: 1.5,
		ValidSamplePct: 100, GOPP95Ms: 4000, AlignmentMaxMs: 8,
		DamagedRanges: []DamagedRangeJSON{
			{StartMs: 4000, EndMs: 5000, Kind: "truncated_box", RepID: "v0", Segment: 5, Line: 12},
		},
	}
}

func TestJSONLRoundTripAndSelfHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	w, err := NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(sampleRecord("clean")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(sampleRecord("duplicate")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("records=%d", len(got))
	}
	for _, r := range got {
		if len(r.ReportSHA256) != 64 {
			t.Fatalf("self hash missing/wrong: %q", r.ReportSHA256)
		}
	}
}

func TestContentHashStableAcrossReruns(t *testing.T) {
	write := func() string {
		path := filepath.Join(t.TempDir(), "run.jsonl")
		w, _ := NewWriter(path)
		_, _ = w.Append(sampleRecord("clean"))
		_, _ = w.Append(sampleRecord("reorder"))
		_ = w.Close()
		w2, _ := NewWriter(path) // reopen truncates, same logical content
		_, _ = w2.Append(sampleRecord("clean"))
		_, _ = w2.Append(sampleRecord("reorder"))
		_ = w2.Close()
		return w2.ContentHash()
	}
	h1 := write()
	h2 := write()
	if h1 == "" || h1 != h2 {
		t.Fatalf("content hash not reproducible: %q vs %q", h1, h2)
	}
}

func TestSortDamagedRanges(t *testing.T) {
	ranges := []DamagedRangeJSON{
		{RepID: "v1", StartMs: 9000, Kind: "invalid_nal"},
		{RepID: "v1", StartMs: 1000, Kind: "burst_loss"},
		{RepID: "v0", StartMs: 1000, Kind: "truncated_box"},
	}
	SortDamagedRanges(ranges)
	if ranges[0].RepID != "v0" || ranges[1].StartMs != 1000 || ranges[2].StartMs != 9000 {
		t.Fatalf("order wrong: %+v", ranges)
	}
}

func TestSelfHashExcludesItself(t *testing.T) {
	// Two records identical apart from a prefilled self-hash must end with the
	// same computed hash, proving the field is not part of its own input.
	r1 := sampleRecord("x")
	r2 := sampleRecord("x")
	r2.ReportSHA256 = "deadbeef"
	w1, _ := NewWriter(filepath.Join(t.TempDir(), "a.jsonl"))
	_, _ = w1.Append(r1)
	w2, _ := NewWriter(filepath.Join(t.TempDir(), "b.jsonl"))
	_, _ = w2.Append(r2)
	if w1.ContentHash() != w2.ContentHash() {
		t.Fatal("self-hash field leaked into content hash")
	}
}
