package checkpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sample(idx int) Entry {
	return Entry{
		Index:          idx,
		URI:            "media/seg-" + strings.Repeat("0", 3) + string(rune('a'+idx)) + ".m4s",
		InputSHA:       strings.Repeat("a", 64),
		MappingVersion: "v1",
		OutputSHA:      strings.Repeat("b", 64),
		OutputBytes:    1024,
		Tracks:         2,
	}
}

func TestCommitLoad(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := sample(3)
	if err := s.Commit(e); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Load(3)
	if err != nil || !ok {
		t.Fatalf("Load = %+v %v %v", got, ok, err)
	}
	if got != e {
		t.Fatalf("roundtrip mismatch: %+v != %+v", got, e)
	}
	if _, ok, _ := s.Load(4); ok {
		t.Fatal("uncommitted slot must be absent")
	}
}

func TestEntriesOrdered(t *testing.T) {
	s, _ := Open(t.TempDir())
	for _, i := range []int{5, 0, 2} {
		if err := s.Commit(sample(i)); err != nil {
			t.Fatal(err)
		}
	}
	es, err := s.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 3 || es[0].Index != 0 || es[1].Index != 2 || es[2].Index != 5 {
		t.Fatalf("bad ordering: %+v", es)
	}
}

func TestRejectsInvalid(t *testing.T) {
	s, _ := Open(t.TempDir())
	cases := []Entry{
		{Index: -1, URI: "a", MappingVersion: "v1", InputSHA: strings.Repeat("a", 64), OutputSHA: strings.Repeat("b", 64), OutputBytes: 1},
		{Index: 0, MappingVersion: "v1", InputSHA: strings.Repeat("a", 64), OutputSHA: strings.Repeat("b", 64), OutputBytes: 1},
		{Index: 0, URI: "a", InputSHA: strings.Repeat("a", 64), OutputSHA: strings.Repeat("b", 64), OutputBytes: 1},
		{Index: 0, URI: "a", MappingVersion: "v1", InputSHA: "short", OutputSHA: strings.Repeat("b", 64), OutputBytes: 1},
		{Index: 0, URI: "a", MappingVersion: "v1", InputSHA: strings.Repeat("a", 64), OutputSHA: strings.Repeat("z", 64), OutputBytes: 1},
		{Index: 0, URI: "a", MappingVersion: "v1", InputSHA: strings.Repeat("a", 64), OutputSHA: strings.Repeat("b", 64), OutputBytes: 0},
	}
	for i, e := range cases {
		if err := s.Commit(e); err == nil {
			t.Errorf("case %d: expected rejection", i)
		}
	}
}

func TestIgnoringTempFiles(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	if err := s.Commit(sample(1)); err != nil {
		t.Fatal(err)
	}
	// Simulate an interrupted commit: a leftover temp file.
	if err := os.WriteFile(filepath.Join(dir, "commit-123.tmp"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	es, err := s.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || es[0].Index != 1 {
		t.Fatalf("temp file must be ignored: %+v", es)
	}
}

func TestIndexMismatchDetected(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	if err := s.Commit(sample(1)); err != nil {
		t.Fatal(err)
	}
	// Copy entry 1 into slot 2: the embedded index betrays the mismatch.
	raw, err := os.ReadFile(filepath.Join(dir, "seg-000000001.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seg-000000002.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Load(2); err == nil {
		t.Fatal("index mismatch must be rejected")
	}
}
