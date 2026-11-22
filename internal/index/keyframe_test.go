package index_test

import (
	"testing"

	"mediapack/internal/fault"
	"mediapack/internal/fixture"
	"mediapack/internal/index"
)

func TestParseHappyPath(t *testing.T) {
	blob := fixture.BuildIndex([]fixture.IndexEntry{
		{FrameSeq: 0, Offset: 0},
		{FrameSeq: 4, Offset: 120},
		{FrameSeq: 8, Offset: 260},
	})
	entries, reports, err := index.Parse(blob, 1024)
	if err != nil || len(reports) != 0 || len(entries) != 3 {
		t.Fatalf("entries=%v reports=%v err=%v", entries, reports, err)
	}
	if entries[2].FrameSeq != 8 || entries[2].Offset != 260 {
		t.Fatalf("record decode wrong: %+v", entries[2])
	}
}

func TestRejectsBadMagicAndVersion(t *testing.T) {
	bad := append([]byte(nil), []byte("XXXXXXX")...)
	bad = append(bad, 1)
	var err error
	if _, _, err = index.Parse(bad, 100); err == nil {
		t.Fatal("bad magic must fail")
	}
	good := fixture.BuildIndex(nil)
	good[len(index.Magic)] = 9
	_, _, err = index.Parse(good, 100)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindCorruptInput {
		t.Fatalf("version check: %v", err)
	}
}

func TestRejectsOutOfRangeOffset(t *testing.T) {
	blob := fixture.BuildIndex([]fixture.IndexEntry{
		{FrameSeq: 0, Offset: 0},
		{FrameSeq: 4, Offset: 5000},
	})
	_, _, err := index.Parse(blob, 1024)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindCorruptInput {
		t.Fatalf("expected corruption, got %v", err)
	}
	if fe.Offset != int64(len(index.Magic)+1+index.EntrySize+8) {
		t.Fatalf("offset field position wrong: %d", fe.Offset)
	}
}

func TestAnomaliesAreSoft(t *testing.T) {
	blob := fixture.BuildIndex([]fixture.IndexEntry{
		{FrameSeq: 0, Offset: 0},
		{FrameSeq: 4, Offset: 200},
		{FrameSeq: 4, Offset: 150}, // duplicate + non-monotonic offset
		{FrameSeq: 8, Offset: 400},
	})
	entries, reports, err := index.Parse(blob, 1024)
	if err != nil || len(entries) != 4 {
		t.Fatalf("soft anomalies must not hard-fail: %v", err)
	}
	kinds := map[index.Anomaly]bool{}
	for _, r := range reports {
		kinds[r.Anomaly] = true
	}
	if !kinds[index.AnomalyDuplicate] || !kinds[index.AnomalyNonMonotonic] {
		t.Fatalf("missing anomaly reports: %+v", reports)
	}
}

func TestTruncatedRecord(t *testing.T) {
	blob := fixture.BuildIndex([]fixture.IndexEntry{{FrameSeq: 0, Offset: 0}})
	blob = blob[:len(blob)-3]
	if _, _, err := index.Parse(blob, 100); err == nil {
		t.Fatal("partial trailing record must fail")
	}
}
