package txn

import (
	"testing"

	"mediarun/internal/clock"
)

// TestInterruptedRunMatchesContinuousRun models the acceptance requirement:
// a run killed at an arbitrary point, then resumed, must end with exactly
// the same committed hashes as a run that was never interrupted.
func TestInterruptedRunMatchesContinuousRun(t *testing.T) {
	names := []string{"000", "001", "002", "003"}

	continuous := t.TempDir()
	for i, name := range names {
		commitOne(t, continuous, name, clock.Tick(i*90000), clock.Tick((i+1)*90000))
	}

	// Interrupted run: commit the first two, stage the third, then die
	// between the commit record and the manifest write.
	interrupted := t.TempDir()
	for i := 0; i < 2; i++ {
		commitOne(t, interrupted, names[i], clock.Tick(i*90000), clock.Tick((i+1)*90000))
	}
	run, err := NewCommitRun(interrupted)
	if err != nil {
		t.Fatal(err)
	}
	seg, err := run.StageSegment(evPayload("002", 180000, 270000))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.WAL.Append(Record{Op: OpCommit, Segment: "002", Hash: seg.SHA256}); err != nil {
		t.Fatal(err)
	}
	// A fourth segment is staged and abandoned: it must never be adopted.
	if _, err := run.StageSegment(evPayload("003", 270000, 360000)); err != nil {
		t.Fatal(err)
	}
	run.Close()

	rec, err := Recover(interrupted)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Committed != 3 {
		t.Fatalf("recovered commits %d", rec.Committed)
	}
	if rec.RolledBack != 1 {
		t.Fatalf("rolled back %d (want the abandoned 003)", rec.RolledBack)
	}
	m, err := LoadManifest(interrupted)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) != 3 {
		t.Fatalf("manifest: %+v", m.Segments)
	}
	// Resume the run and finish segment 003.
	commitOne(t, interrupted, "003", 270000, 360000)

	final, err := LoadManifest(interrupted)
	if err != nil {
		t.Fatal(err)
	}
	want, err := LoadManifest(continuous)
	if err != nil {
		t.Fatal(err)
	}
	if !CompareHashes(final, want) {
		t.Fatalf("recovered run differs from continuous run:\n%+v\n%+v", final.Segments, want.Segments)
	}
	if len(final.Segments) != len(names) {
		t.Fatalf("segments %d", len(final.Segments))
	}
}
