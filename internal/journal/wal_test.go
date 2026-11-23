package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWALAppendAndReload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "wal.log")
	w, err := Open(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	must := func(r Record) {
		if err := w.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	must(Record{Stage: "manifest", Status: StatusBegin, InputSHA: "a"})
	must(Record{Stage: "manifest", Status: StatusCommit, InputSHA: "a", OutputSHA: "b"})
	must(Record{Stage: "transport", Status: StatusBegin, InputSHA: "b"})
	w.Close()

	w2, err := Open(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	recs := w2.Records()
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	c, ok := w2.Committed("manifest")
	if !ok || c.OutputSHA != "b" {
		t.Fatalf("commit not found: %+v", c)
	}
	if _, ok := w2.Committed("transport"); ok {
		t.Fatal("transport was never committed")
	}
}

func TestWALCompaction(t *testing.T) {
	p := filepath.Join(t.TempDir(), "wal.log")
	w, err := Open(p, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := 0; i < 6; i++ {
		stage := string(rune('a' + i))
		if err := w.Append(Record{Stage: stage, Status: StatusBegin}); err != nil {
			t.Fatal(err)
		}
		if err := w.Append(Record{Stage: stage, Status: StatusCommit, OutputSHA: stage}); err != nil {
			t.Fatal(err)
		}
	}
	recs := w.Records()
	if len(recs) > 8 {
		t.Fatalf("log grew past limit: %d records", len(recs))
	}
	// Every stage's commit survives compaction.
	for i := 0; i < 6; i++ {
		if _, ok := w.Committed(string(rune('a' + i))); !ok {
			t.Fatalf("commit for stage %c lost in compaction", 'a'+i)
		}
	}
	// File on disk matches.
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() == 0 {
		t.Fatal("compacted wal is empty")
	}
}
