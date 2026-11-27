package journal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointRoundTripAndReuse(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cp, err := st.LoadCheckpoint(); err != nil || cp != nil {
		t.Fatalf("empty dir: %v %v", cp, err)
	}
	hash, err := st.WriteArtifact("repair", []byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	cp := Checkpoint{InputDigest: "abc", Completed: []string{"parse", "repair"}, Hashes: map[string]string{"repair": hash}}
	if err := st.SaveCheckpoint(cp); err != nil {
		t.Fatal(err)
	}
	got := st.Reused("abc")
	if len(got) != 1 || got[0] != "repair" {
		t.Fatalf("reused: %+v", got)
	}
	if st.Reused("other") != nil {
		t.Fatal("digest mismatch must not reuse")
	}
	if err := os.WriteFile(filepath.Join(st.Dir, "stage_repair.json"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st.Reused("abc") != nil {
		t.Fatal("tampered artifact must not be reused")
	}
}

func TestLoadCorruptCheckpoint(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(st.Dir, "checkpoint.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadCheckpoint(); !errors.Is(err, ErrCorruptCheckpoint) {
		t.Fatalf("err: %v", err)
	}
}

func TestJournalAppendAndRead(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := st.AppendRecord(Record{Seq: i, Stage: "repair", InputDigest: "d", Segment: "v1"}); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := st.Records()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[2].Seq != 2 || recs[0].Version != Version {
		t.Fatalf("records: %+v", recs)
	}
}

func TestAtomicWriteAndDigestStability(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	if err := AtomicWrite(path, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "payload" {
		t.Fatalf("read back: %q %v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary file left behind: %+v", entries)
	}
	in := t.TempDir()
	if err := os.MkdirAll(filepath.Join(in, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in, "a.bin"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in, "sub", "b.bin"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	d1, err := DigestInput(in)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := DigestInput(in)
	if err != nil || d1 != d2 || len(d1) != 64 {
		t.Fatalf("digest: %q %q %v", d1, d2, err)
	}
	if err := os.WriteFile(filepath.Join(in, "sub", "b.bin"), []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}
	d3, err := DigestInput(in)
	if err != nil || d3 == d1 {
		t.Fatal("digest must change with content")
	}
}
