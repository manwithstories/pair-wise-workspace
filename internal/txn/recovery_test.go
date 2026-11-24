package txn

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediarun/internal/clock"
	"mediarun/internal/model"
)

func commitOne(t *testing.T, dir, name string, start, end clock.Tick) SegmentCommit {
	t.Helper()
	run, err := NewCommitRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	seg, err := run.StageSegment(evPayload(name, start, end))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.CommitSegment(seg); err != nil {
		t.Fatal(err)
	}
	return seg
}

func TestRecoverCleanRun(t *testing.T) {
	dir := t.TempDir()
	seg := commitOne(t, dir, "000", 0, 90000)
	rec, err := Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Committed != 1 || rec.HashMatch != 1 || rec.HashMismatch != 0 {
		t.Fatalf("recovery: %+v", rec)
	}
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) != 1 || m.Segments[0].SHA256 != seg.SHA256 {
		t.Fatalf("manifest: %+v", m.Segments)
	}
}

func TestRecoverRollsBackStagedOnly(t *testing.T) {
	dir := t.TempDir()
	commitOne(t, dir, "000", 0, 90000)
	// Stage a second segment and "crash" before committing it.
	run, _ := NewCommitRun(dir)
	run.Begin("compose")
	if _, err := run.StageSegment(evPayload("001", 90000, 180000)); err != nil {
		t.Fatal(err)
	}
	run.Close() // no CommitSegment

	rec, err := Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rec.RolledBack != 1 {
		t.Fatalf("rolled back %d", rec.RolledBack)
	}
	m, _ := LoadManifest(dir)
	if len(m.Segments) != 1 || m.Segments[0].Name != "000" {
		t.Fatalf("manifest kept: %+v", m.Segments)
	}
	// Repeated recovery is identical.
	rec2, err := Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rec2.Committed != rec.Committed || rec2.RolledBack != rec.RolledBack {
		t.Fatalf("recovery not idempotent: %+v vs %+v", rec, rec2)
	}
}

func TestRecoverAfterCommitRecordBeforeManifest(t *testing.T) {
	dir := t.TempDir()
	run, _ := NewCommitRun(dir)
	seg, err := run.StageSegment(evPayload("000", 0, 90000))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash between the WAL commit record and the manifest
	// write: append the commit record directly.
	if err := run.WAL.Append(Record{Op: OpCommit, Segment: "000", Hash: seg.SHA256}); err != nil {
		t.Fatal(err)
	}
	run.Close()
	// The manifest does not yet contain the segment.
	before, _ := LoadManifest(dir)
	if len(before.Segments) != 0 {
		t.Fatalf("manifest should be empty: %+v", before.Segments)
	}
	rec, err := Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Committed != 1 {
		t.Fatalf("recovery: %+v", rec)
	}
	// The commit record is durable even though the manifest write was lost,
	// so replay must adopt the segment and converge with a clean run.
	if len(rec.Segments) != 1 || rec.Segments[0] != "000" {
		t.Fatalf("segments: %+v", rec.Segments)
	}
	after, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Segments) != 1 || after.Segments[0].SHA256 != seg.SHA256 {
		t.Fatalf("adopted manifest: %+v", after.Segments)
	}
	if after.Segments[0].Events != 1 || after.Segments[0].End != 90000 {
		t.Fatalf("adopted metadata: %+v", after.Segments[0])
	}
	// A second replay is a no-op.
	rec2, err := Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rec2.Committed != 1 || rec2.HashMismatch != 0 {
		t.Fatalf("second replay: %+v", rec2)
	}
}

func TestRecoverDetectsTamperedSegment(t *testing.T) {
	dir := t.TempDir()
	commitOne(t, dir, "000", 0, 90000)
	path := filepath.Join(dir, SegmentFile("000"))
	if err := os.WriteFile(path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The WAL still points at the old hash, so replay reports a change.
	if _, err := Recover(dir); err == nil {
		t.Fatal("tampered segment accepted")
	}
}

func TestRecoverDropsManifestEntryWithMissingFile(t *testing.T) {
	dir := t.TempDir()
	commitOne(t, dir, "000", 0, 90000)
	if err := os.Remove(filepath.Join(dir, SegmentFile("000"))); err != nil {
		t.Fatal(err)
	}
	// Remove the WAL so Replay sees no commit records; the manifest entry
	// must still be dropped because its file is gone.
	if err := Reset(dir); err != nil {
		t.Fatal(err)
	}
	rec, err := Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rec.HashMismatch != 1 || rec.HashMatch != 0 {
		t.Fatalf("recovery: %+v", rec)
	}
	m, _ := LoadManifest(dir)
	if len(m.Segments) != 0 {
		t.Fatalf("manifest should be empty: %+v", m.Segments)
	}
}

func TestContinuousVsRecoveredHashes(t *testing.T) {
	continuous := t.TempDir()
	recovered := t.TempDir()
	names := []string{"000", "001", "002"}
	for i, name := range names {
		commitOne(t, continuous, name, clock.Tick(i*90000), clock.Tick((i+1)*90000))
	}
	// Recovered run: commit 000 and 001, stage 002, then abandon.
	for i := 0; i < 2; i++ {
		commitOne(t, recovered, names[i], clock.Tick(i*90000), clock.Tick((i+1)*90000))
	}
	run, _ := NewCommitRun(recovered)
	if _, err := run.StageSegment(evPayload("002", 180000, 270000)); err != nil {
		t.Fatal(err)
	}
	run.Close()
	if _, err := Recover(recovered); err != nil {
		t.Fatal(err)
	}
	// Finish the recovered run.
	commitOne(t, recovered, "002", 180000, 270000)

	a, _ := LoadManifest(continuous)
	b, _ := LoadManifest(recovered)
	if !CompareHashes(a, b) {
		t.Fatalf("committed hashes diverge:\n%+v\n%+v", a.Segments, b.Segments)
	}
}

func TestSegmentPayloadEncodeDeterministic(t *testing.T) {
	p := NewSegmentPayload("000", 0, 90000,
		[]model.Event{
			{Kind: model.KindFrame, TrackID: 2, Start: 0, Duration: 1000},
			{Kind: model.KindSubtitle, TrackID: 1, Start: 0, Duration: 1000},
		},
		[]model.Gap{{TrackID: 1, Count: 2}})
	first, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	second, _ := p.Encode()
	if string(first) != string(second) {
		t.Fatal("payload encoding not deterministic")
	}
	sum := sha256.Sum256(first)
	if hex.EncodeToString(sum[:]) != strings.ToLower(hex.EncodeToString(sum[:])) {
		t.Fatal("hash format")
	}
	if len(p.TrackIDs) != 2 || p.TrackIDs[0] != 1 {
		t.Fatalf("track ids: %v", p.TrackIDs)
	}
	bad := *p
	bad.Version = 7
	if _, err := bad.Encode(); err == nil {
		t.Fatal("bad version accepted")
	}
}
