package txn

import (
	"os"
	"path/filepath"
	"testing"

	"relaycheck/internal/media"
)

func TestWALAppendReadAndLSN(t *testing.T) {
	dir := t.TempDir()
	w, f := OpenWAL(dir)
	if f != nil {
		t.Fatal(f)
	}
	if _, f := w.Append(Record{Phase: PhaseBegin, Segment: 0}); f != nil {
		t.Fatal(f)
	}
	if _, f := w.Append(Record{Phase: PhasePrepared, Segment: 0, SHA256: "abc"}); f != nil {
		t.Fatal(f)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	w2, f := OpenWAL(dir)
	if f != nil {
		t.Fatal(f)
	}
	recs, f := w2.ReadAll()
	if f != nil {
		t.Fatal(f)
	}
	if len(recs) != 2 || recs[0].LSN != 1 || recs[1].LSN != 2 || recs[1].SHA256 != "abc" {
		t.Fatalf("bad records: %+v", recs)
	}
	_ = w2.Close()
}

func TestWALCorruptLineDetected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wal.log"), []byte("{not json}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, f := OpenWAL(dir)
	if f != nil {
		t.Fatal(f)
	}
	defer w.Close()
	if _, f := w.ReadAll(); f == nil || f.Code != media.CodeWALCorrupt {
		t.Fatalf("want wal_corrupt, got %v", f)
	}
}

func TestCommitSegmentWritesContentAddressedFile(t *testing.T) {
	c, f := NewCommitter(t.TempDir())
	if f != nil {
		t.Fatal(f)
	}
	defer c.Close()
	body := []byte("canonical-segment-bytes")
	sha, f := c.CommitSegment(0, body)
	if f != nil {
		t.Fatal(f)
	}
	got, err := os.ReadFile(filepath.Join(c.Dir, SegmentFileName(0)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) || sha != shaHex(body) {
		t.Fatal("committed bytes/hash mismatch")
	}
}

func TestRecoverPendingAndResolve(t *testing.T) {
	c, f := NewCommitter(t.TempDir())
	if f != nil {
		t.Fatal(f)
	}
	defer c.Close()
	body3 := []byte("segment-three-deterministic")

	// segments 0..2 fully commit
	for i := 0; i < 3; i++ {
		if _, f := c.CommitSegment(i, []byte{byte('a' + i)}); f != nil {
			t.Fatal(f)
		}
	}
	// segment 3 reaches durable prepared state then "crashes" before rename
	sha3, f := c.PrepareSegment(3, body3)
	if f != nil {
		t.Fatal(f)
	}

	pending, f := c.RecoverPending()
	if f != nil {
		t.Fatal(f)
	}
	if len(pending) != 1 || pending[0].Segment != 3 || pending[0].SHA != sha3 {
		t.Fatalf("pending=%+v", pending)
	}
	if _, err := os.Stat(c.finalPath(3)); !os.IsNotExist(err) {
		t.Fatal("segment 3 must not be atomically committed before replay")
	}

	if f := c.ResolvePending(pending[0], body3); f != nil {
		t.Fatal(f)
	}
	got, err := os.ReadFile(c.finalPath(3))
	if err != nil || string(got) != string(body3) {
		t.Fatalf("replayed segment wrong: %v err=%v", got, err)
	}
	pending2, f := c.RecoverPending()
	if f != nil || len(pending2) != 0 {
		t.Fatalf("pending after resolve=%+v f=%v", pending2, f)
	}
}

func TestResolvePendingHashMismatchIsIntegrity(t *testing.T) {
	c, _ := NewCommitter(t.TempDir())
	defer c.Close()
	sha, f := c.PrepareSegment(4, []byte("original"))
	if f != nil {
		t.Fatal(f)
	}
	p := PendingTxn{Segment: 4, SHA: sha}
	fault := c.ResolvePending(p, []byte("tampered-deterministic-bytes"))
	if fault == nil || fault.Code != media.CodeChecksumMismatch || fault.ExitCode() != 3 {
		t.Fatalf("want checksum_mismatch exit3, got %v", fault)
	}
}

func TestWriteManifestAtomicAndReadable(t *testing.T) {
	c, _ := NewCommitter(t.TempDir())
	defer c.Close()
	m := &Manifest{Seed: 25, TargetRate: 48000, Layout: "stereo", SegmentCount: 1,
		Entries: []ManifestEntry{{Index: 0, File: SegmentFileName(0), SHA256: "x"}}}
	if f := c.WriteManifest(m); f != nil {
		t.Fatal(f)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "manifest.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("tmp manifest must be renamed away")
	}
}
