package txn

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"relay/internal/media"
)

func sha256Bytes(p []byte) [32]byte { return sha256.Sum256(p) }

func hexOf(id [32]byte) string { return hex.EncodeToString(id[:]) }

func frame(seq uint64, payload string) *media.RawFrame {
	return &media.RawFrame{
		Kind: media.KindVideo,
		Meta: media.FrameMeta{
			SourceID: 1, Sequence: seq, Size: uint32(len(payload)),
			SHA256: sha256Bytes([]byte(payload)),
		},
		Payload: []byte(payload),
	}
}

func TestWALAppendReadChecksum(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := [32]byte{1, 2, 3}
	if _, err := w.Append(RecBegin, id[:]); err != nil {
		t.Fatal(err)
	}
	rec := StageRecord{TrackID: 1, Sequence: 9, Offset: 0, Size: 4, RelPath: "staging/x.bin"}
	rec.SHA256 = sha256Bytes([]byte("data"))
	if _, err := w.Append(RecStage, EncodeStage(rec)); err != nil {
		t.Fatal(err)
	}
	recs, err := w.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Type != RecBegin || recs[1].Type != RecStage {
		t.Fatalf("unexpected records: %+v", recs)
	}
	dec, err := DecodeStage(recs[1].Payload)
	if err != nil || dec.Sequence != 9 || dec.RelPath != "staging/x.bin" {
		t.Fatalf("stage decode failed: %+v %v", dec, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWALCorruptionDetected(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(RecCommit, []byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	w.Close()
	path := filepath.Join(dir, "wal.log")
	data, _ := os.ReadFile(path)
	data[10] ^= 0xff // flip a payload bit
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	w2, _ := OpenWAL(dir)
	defer w2.Close()
	if _, err := w2.ReadAll(); err == nil {
		t.Fatal("want checksum error on corrupted WAL")
	}
}

func TestCommitAndCurrent(t *testing.T) {
	dir := t.TempDir()
	id := [32]byte{9, 9, 9}
	tr, err := Begin(dir, id, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Stage(1, 0, frame(0, "alpha")); err != nil {
		t.Fatal(err)
	}
	if err := tr.Stage(1, 1, frame(1, "bravo")); err != nil {
		t.Fatal(err)
	}
	if err := tr.Finalize(); err != nil {
		t.Fatal(err)
	}
	cur, ok := CurrentCommitID(dir)
	if !ok || cur != id {
		t.Fatalf("CURRENT wrong: %x ok=%v", cur, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, "staging")); !os.IsNotExist(err) {
		t.Fatal("staging dir should be removed after commit")
	}
}

func TestRollbackLeavesCurrentUntouched(t *testing.T) {
	dir := t.TempDir()
	original := [32]byte{0xaa}
	if err := os.WriteFile(filepath.Join(dir, "CURRENT"),
		append(append([]byte(nil), hexOf(original)...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := Begin(dir, [32]byte{0xbb}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Stage(1, 0, frame(0, "x")); err != nil {
		t.Fatal(err)
	}
	if err := tr.Rollback(); err != nil {
		t.Fatal(err)
	}
	cur, ok := CurrentCommitID(dir)
	if !ok || cur != original {
		t.Fatalf("CURRENT changed after rollback: %x ok=%v", cur, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, "staging")); !os.IsNotExist(err) {
		t.Fatal("staging must be cleaned on rollback")
	}
}

func TestResumeCommitsOnceAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	id := [32]byte{7, 7, 7}
	// Simulate a crash after staging: Begin + stages + manifest, no commit.
	w, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(RecBegin, id[:]); err != nil {
		t.Fatal(err)
	}
	fr := frame(0, "payload")
	stagingPath := filepath.Join(dir, "staging", "track-1", "sample-0000000000.bin")
	if err := os.MkdirAll(filepath.Dir(stagingPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagingPath, fr.Payload, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := StageRecord{TrackID: 1, Sequence: 0, Size: 7, RelPath: "staging/track-1/sample-0000000000.bin"}
	rec.SHA256 = sha256Bytes(fr.Payload)
	if _, err := w.Append(RecStage, EncodeStage(rec)); err != nil {
		t.Fatal(err)
	}
	m := Manifest{CommitID: id, Entries: []ManifestEntry{{
		TrackID: 1, Sequence: 0, Size: 7, SHA256: rec.SHA256,
	}}}
	if _, err := w.Append(RecManifest, EncodeManifest(m)); err != nil {
		t.Fatal(err)
	}
	w.Close()

	sum1, err := Resume(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !sum1.Committed || sum1.Replayed != 1 {
		t.Fatalf("first resume wrong: %+v", sum1)
	}
	cur, ok := CurrentCommitID(dir)
	if !ok || cur != id {
		t.Fatal("CURRENT not published by resume")
	}
	// Second resume must not create a second commit and must stay identical.
	sum2, err := Resume(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !sum2.Committed || sum2.ManifestSum != sum1.ManifestSum {
		t.Fatalf("second resume not idempotent: %+v", sum2)
	}
}

func TestCheckWithinRejectsEscape(t *testing.T) {
	if err := checkWithin(t.TempDir(), filepath.Join(t.TempDir(), "..", "escape")); err == nil {
		t.Fatal("path escape must be rejected")
	}
}
