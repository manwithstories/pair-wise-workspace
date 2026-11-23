package index

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommitValidateAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "idx"))
	if err != nil {
		t.Fatal(err)
	}
	data := EncodeEntries([]Entry{
		{Track: "v1", Seq: 0, DTSus: 0, PTSus: 0, ByteOffset: 100, Size: 50, PrevDTSus: -1},
		{Track: "v1", Seq: 9, DTSus: 90000, PTSus: 90000, ByteOffset: 900, Size: 50, PrevDTSus: 0},
	})
	ok, err := st.CommitSegment("v1-0", data)
	if err != nil || !ok {
		t.Fatalf("first commit: ok=%v err=%v", ok, err)
	}
	// Idempotent: identical re-commit is a no-op, no duplicate segment.
	ok, err = st.CommitSegment("v1-0", data)
	if err != nil || ok {
		t.Fatalf("recommit should be no-op: ok=%v err=%v", ok, err)
	}
	matches, _ := filepath.Glob(filepath.Join(st.dir, "seg-*.idx"))
	if len(matches) != 1 {
		t.Fatalf("segments=%d want 1", len(matches))
	}
	// No temp files left behind.
	left, _ := filepath.Glob(filepath.Join(st.dir, "*.tmp"))
	if len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	// Conflicting content for the same key is a corrupt error.
	if _, err := st.CommitSegment("v1-0", []byte("different")); err == nil {
		t.Fatal("conflicting segment accepted")
	}
}

func TestFinalizeDeterministic(t *testing.T) {
	build := func() string {
		dir := t.TempDir()
		st, _ := Open(dir)
		st.CommitSegment("b", EncodeEntries([]Entry{{Track: "b", Seq: 1}}))
		st.CommitSegment("a", EncodeEntries([]Entry{{Track: "a", Seq: 2}}))
		sha, err := st.Finalize()
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}
	if build() != build() {
		t.Fatal("index digest not deterministic across runs")
	}
}

func TestEncodeEntriesSorted(t *testing.T) {
	a := EncodeEntries([]Entry{
		{Track: "v1", DTSus: 5}, {Track: "a1", DTSus: 9}, {Track: "v1", DTSus: 1},
	})
	b := EncodeEntries([]Entry{
		{Track: "v1", DTSus: 1}, {Track: "v1", DTSus: 5}, {Track: "a1", DTSus: 9},
	})
	if string(a) != string(b) {
		t.Fatal("encoding not canonical")
	}
	if _, err := os.Stat("/nonexistent"); err == nil {
		t.Fatal("sanity")
	}
}
