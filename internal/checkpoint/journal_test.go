package checkpoint_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"avpipe/internal/checkpoint"
)

func commitTestChunk(t *testing.T, dir string, seq int, body string) (*checkpoint.Store, *checkpoint.ChunkRecord) {
	t.Helper()
	st, err := checkpoint.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rel := "segments/r0/seg_000.ts"
	sr, err := st.CommitSegment(rel, strings.NewReader(body), checkpoint.SegmentRecord{
		Seq: seq, ChunkSeq: seq, Rendition: "r0", Bitrate: 600000,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := &checkpoint.ChunkRecord{
		Seq: seq, SourceHash: "source", ConfigHash: "config", Segments: []checkpoint.SegmentRecord{sr},
	}
	if err := st.CommitChunk(rec); err != nil {
		t.Fatal(err)
	}
	return st, rec
}

func TestCheckpointCommitRestartAndOrphanCleanup(t *testing.T) {
	dir := t.TempDir()
	_, rec := commitTestChunk(t, dir, 0, "deterministic-segment-bytes")

	orphanTmp := filepath.Join(dir, "segments", ".tmp-crashed")
	orphanFinal := filepath.Join(dir, "segments", "r0", "orphan.ts")
	if err := os.WriteFile(orphanTmp, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanFinal, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}

	st2, err := checkpoint.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx, verified, bad, removed, err := st2.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 || verified != 1 {
		t.Fatalf("verified=%d bad=%v", verified, bad)
	}
	if len(idx.Chunks) != 1 || idx.Chunks[0].Segments[0].SHA256 != rec.Segments[0].SHA256 {
		t.Fatalf("index recovery mismatch: %+v", idx.Chunks)
	}
	if len(removed) != 2 {
		t.Fatalf("removed=%v want two orphans", removed)
	}
	if _, err := os.Stat(orphanTmp); !os.IsNotExist(err) {
		t.Fatal("temp orphan survived recovery")
	}
	if _, err := os.Stat(orphanFinal); !os.IsNotExist(err) {
		t.Fatal("uncommitted final orphan survived recovery")
	}

	seqs, err := checkpoint.ReadJournalSeqs(filepath.Join(dir, checkpoint.JournalName))
	if err != nil || len(seqs) != 1 || seqs[0] != 0 {
		t.Fatalf("journal seqs=%v err=%v", seqs, err)
	}
}

func TestCheckpointDetectsCorruptCommittedFile(t *testing.T) {
	dir := t.TempDir()
	st, rec := commitTestChunk(t, dir, 3, "hash-me")
	full, err := checkpoint.SafePath(dir, rec.Segments[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := st.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	verified, bad, err := st.Verify(idx)
	if err != nil {
		t.Fatal(err)
	}
	if verified != 0 || len(bad) != 1 {
		t.Fatalf("verified=%d bad=%v", verified, bad)
	}
}

func TestSafePathRejectsUnsafeRecord(t *testing.T) {
	if _, err := checkpoint.SafePath(t.TempDir(), "../../etc/passwd"); err == nil {
		t.Fatal("path traversal accepted")
	}
}
