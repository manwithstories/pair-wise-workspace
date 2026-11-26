package provenance

import (
	"path/filepath"
	"testing"
)

func TestTraceWriteLoad(t *testing.T) {
	tr, err := Open(filepath.Join(t.TempDir(), "prov"))
	if err != nil {
		t.Fatal(err)
	}
	recs1 := []SegmentTrace{
		{Seq: 1, File: "seg_000001.m4s", Decision: Accepted, InputSHA: "a",
			Tracks: []TrackTrace{{TrackID: 1, Samples: 30, FirstDTS: 0, Duration: 90000, PayloadSHA: "p", OutputSHA: "p"}}},
		{Seq: 2, Decision: Lost, Repairs: []string{"timeline skipped by estimate"}},
	}
	if err := tr.WriteChunk(1, recs1); err != nil {
		t.Fatal(err)
	}
	if err := tr.WriteChunk(2, []SegmentTrace{{Seq: 33, Decision: Quarantined}}); err != nil {
		t.Fatal(err)
	}
	// rewriting chunk 1 replaces rather than duplicates
	if err := tr.WriteChunk(1, recs1); err != nil {
		t.Fatal(err)
	}
	got, err := tr.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("records=%d", len(got))
	}
	if got[0].Seq != 1 || got[0].Tracks[0].PayloadSHA != "p" || got[2].Decision != Quarantined {
		t.Fatalf("got=%+v", got)
	}
}
