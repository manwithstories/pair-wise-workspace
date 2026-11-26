package txn

import (
	"os"
	"path/filepath"
	"testing"

	"mediamix/internal/clock"
)

func tempDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	return d
}

func TestStageCommitAndReplayHash(t *testing.T) {
	dir := tempDir(t)
	c, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	payloads := [][]byte{[]byte("alpha"), []byte("bravo"), []byte("charlie")}
	for _, p := range payloads {
		if _, err := c.Stage("convert", p); err != nil {
			t.Fatalf("stage: %v", err)
		}
	}
	m, err := c.Commit("convert")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if m.Bytes != 17 || len(m.RecordIDs) != 3 {
		t.Fatalf("manifest wrong: %+v", m)
	}
	if c.PendingCount() != 0 {
		t.Fatal("pending should clear after commit")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen and replay: hash must match.
	c2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	manifests, err := c2.Replay()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(manifests) != 1 || manifests[0].SHA256 != m.SHA256 {
		t.Fatalf("replay manifest mismatch: %+v", manifests)
	}
}

func TestRollbackKeepsLastBoundary(t *testing.T) {
	dir := tempDir(t)
	c, _ := Open(dir)
	c.Stage("convert", []byte("committed-one"))
	first, err := c.Commit("convert")
	if err != nil {
		t.Fatal(err)
	}
	// Fault after normalize: stage more records, then roll back.
	c.Stage("loudness", []byte("uncommitted-a"))
	c.Stage("loudness", []byte("uncommitted-b"))
	if err := c.Rollback("injected fault after normalize"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if c.PendingCount() != 0 {
		t.Fatal("rollback must clear pending records")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2, _ := Open(dir)
	got, err := c2.Replay()
	if err != nil {
		t.Fatalf("replay after rollback: %v", err)
	}
	if len(got) != 1 || got[0].SHA256 != first.SHA256 {
		t.Fatalf("rollback did not preserve last good boundary: %+v", got)
	}
}

func TestCorruptTailIsTruncated(t *testing.T) {
	dir := tempDir(t)
	c, _ := Open(dir)
	c.Stage("convert", []byte("good-record"))
	if _, err := c.Commit("convert"); err != nil {
		t.Fatal(err)
	}
	walPath := filepath.Join(dir, walFileName)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Append garbage bytes that look like the start of a new record but fail
	// magic/CRC validation. They must be truncated, not treated as data.
	info, _ := os.Stat(walPath)
	goodSize := info.Size()
	f, err := os.OpenFile(walPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("GARBAGE_NOT_A_RECORD")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Reopen via the WAL directly to observe the dropped-byte count, then
	// via the commit coordinator to confirm replay still succeeds.
	wal2, dropped, err := OpenWAL(dir)
	if err != nil {
		t.Fatalf("reopen corrupt wal: %v", err)
	}
	if dropped == 0 {
		t.Fatal("expected nonzero dropped byte count")
	}
	wal2.Close()
	c2, err := Open(dir)
	if err != nil {
		t.Fatalf("coordinator reopen: %v", err)
	}
	info2, _ := os.Stat(walPath)
	if info2.Size() != goodSize {
		t.Fatalf("wal not truncated to last valid boundary: %d != %d", info2.Size(), goodSize)
	}
	if _, err := c2.Replay(); err != nil {
		t.Fatalf("replay after truncation should succeed: %v", err)
	}
}

func TestEmptyCommitRejected(t *testing.T) {
	dir := tempDir(t)
	c, _ := Open(dir)
	if _, err := c.Commit("convert"); !clock.IsCode(err, clock.ErrInvalidParam) {
		t.Fatalf("empty commit must fail, got %v", err)
	}
}

func TestInvalidPathsRejected(t *testing.T) {
	if _, err := Open(""); !clock.IsCode(err, clock.ErrInvalidPath) {
		t.Fatalf("empty dir must be rejected: %v", err)
	}
	if _, err := Open(filepath.Join(tempDir(t), "..", "..", "escaped")); err == nil {
		t.Fatal("traversal path must be rejected")
	}
	if _, err := Open(filepath.Join(tempDir(t), "afile")); err == nil {
		// Create a regular file where a directory is expected.
	}
	file := filepath.Join(tempDir(t), "notdir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file); !clock.IsCode(err, clock.ErrInvalidPath) {
		t.Fatalf("non-directory path must be rejected, got %v", err)
	}
}

func TestReplayMatchesFaultFreeRun(t *testing.T) {
	// Build the full committed payload set in one clean run, then prove a
	// run that injects a rollback in the middle replays to the same hash.
	dir := tempDir(t)
	all := [][]byte{[]byte("frag-0"), []byte("frag-1"), []byte("frag-2"), []byte("frag-3")}

	// Fault-free reference.
	ref, _ := Open(dir)
	for _, p := range all {
		ref.Stage("convert", p)
	}
	refCommit, err := ref.Commit("convert")
	if err != nil {
		t.Fatal(err)
	}
	ref.Close()

	// Faulty run in a separate dir: commit first half, fault, rollback, then
	// redo the whole transaction. Final committed hash must be identical.
	dir2 := tempDir(t)
	faulty, _ := Open(dir2)
	faulty.Stage("convert", all[0])
	faulty.Stage("convert", all[1])
	good, _ := faulty.Commit("convert")
	_ = good
	faulty.Stage("loudness", []byte("will-be-rolled-back"))
	if err := faulty.Rollback("fault after normalize"); err != nil {
		t.Fatal(err)
	}
	for _, p := range all {
		faulty.Stage("convert", p)
	}
	final, err := faulty.Commit("convert")
	if err != nil {
		t.Fatal(err)
	}
	faulty.Close()

	reopened, _ := Open(dir2)
	manifests, err := reopened.Replay()
	if err != nil {
		t.Fatal(err)
	}
	// The last committed boundary must hash-equal the fault-free reference.
	last := manifests[len(manifests)-1]
	if last.SHA256 != refCommit.SHA256 {
		t.Fatalf("recovered hash %s != fault-free %s", last.SHA256, refCommit.SHA256)
	}
	if final.SHA256 != refCommit.SHA256 {
		t.Fatal("final manifest hash disagrees with reference")
	}
}
