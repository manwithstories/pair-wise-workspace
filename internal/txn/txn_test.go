package txn

import (
	"os"
	"path/filepath"
	"testing"
)

func openWAL(t *testing.T, path string) (*WAL, []Record) {
	t.Helper()
	w, recs, _, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return w, recs
}

func TestCommitAndDeterministicDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	digests := make([]string, 2)
	for run := 0; run < 2; run++ {
		w, recs := openWAL(t, path)
		c := NewCommitter(w, recs, false)
		if run == 0 {
			for i := 0; i < 3; i++ {
				payload := []byte{byte(i), 1, 2, 3}
				if err := c.Submit(NewSegment(uint64(i), int64(i*100), int64(i*100+100), i, payload)); err != nil {
					t.Fatal(err)
				}
			}
		}
		digests[run] = c.Digest()
		w.Close()
	}
	if digests[0] != digests[1] {
		t.Fatalf("digest not reproducible: %s vs %s", digests[0], digests[1])
	}
}

func TestCorruptPayloadRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	w, recs := openWAL(t, path)
	defer w.Close()
	c := NewCommitter(w, recs, false)
	seg := NewSegment(0, 0, 100, 1, []byte("clean"))
	seg.Payload[0] = 'X' // mutate after digest was computed
	if err := c.Submit(seg); err == nil {
		t.Fatal("corrupt segment committed")
	}
	if c.RollbackCount() != 1 {
		t.Fatalf("rollbacks = %d, want 1", c.RollbackCount())
	}
	if len(c.Committed()) != 0 {
		t.Fatalf("corrupt segment present in committed set")
	}
}

func TestTimeRangeViolationRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	w, recs := openWAL(t, path)
	defer w.Close()
	c := NewCommitter(w, recs, false)
	if err := c.Submit(NewSegment(0, 0, 100, 0, []byte("a"))); err != nil {
		t.Fatal(err)
	}
	// Overlaps the committed horizon.
	if err := c.Submit(NewSegment(1, 50, 150, 0, []byte("b"))); err == nil {
		t.Fatal("overlapping segment committed")
	}
	if len(c.Committed()) != 1 {
		t.Fatalf("committed = %d, want 1", len(c.Committed()))
	}
}

func TestTruncatedWALRollsBackUncommitted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	// First process: commit segment 0, then crash mid-segment-1.
	w, recs := openWAL(t, path)
	c := NewCommitter(w, recs, false)
	if err := c.Submit(NewSegment(0, 0, 100, 0, []byte("committed"))); err != nil {
		t.Fatal(err)
	}
	digestBefore := c.Digest()
	crash := NewSegment(1, 100, 200, 0, []byte("uncommitted"))
	if err := w.Append(Record{Type: RecSegment, SegmentID: crash.ID, Payload: crash.marshal()}); err != nil {
		t.Fatal(err)
	}
	// Tear the tail: truncate inside the commit marker that never came.
	info, _ := os.Stat(path)
	if err := os.Truncate(path, info.Size()-3); err != nil {
		t.Fatal(err)
	}
	w.Close()
	// Restart: uncommitted segment must be rolled back, digest unchanged.
	w2, recs2, torn2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	c2 := NewCommitter(w2, recs2, torn2)
	if c2.RollbackCount() != 1 {
		t.Fatalf("rollbacks after restart = %d, want 1", c2.RollbackCount())
	}
	if len(c2.Committed()) != 1 || c2.Digest() != digestBefore {
		t.Fatalf("restart state diverged: committed=%d digest=%s want %s",
			len(c2.Committed()), c2.Digest(), digestBefore)
	}
	// Processing resumes from the last valid commit point.
	if err := c2.Submit(NewSegment(1, 100, 200, 0, []byte("resumed"))); err != nil {
		t.Fatalf("resume after rollback failed: %v", err)
	}
}
