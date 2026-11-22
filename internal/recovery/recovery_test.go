package recovery_test

import (
	"os"
	"testing"

	"mediac/internal/model"
	"mediac/internal/recovery"
	"mediac/internal/segment"
)

func cp(rep string, idx ...int) *recovery.Checkpoint {
	c := &recovery.Checkpoint{RepID: rep, SourceSHA256: "src", ConfigSHA256: "cfg"}
	for _, i := range idx {
		c.Completed = append(c.Completed, segment.SegmentEntry{Index: i})
	}
	return c
}

func TestCheckpointRoundtrip(t *testing.T) {
	dir := t.TempDir()
	if merr := recovery.Save(dir, cp("rep1", 0, 1)); merr != nil {
		t.Fatal(merr)
	}
	if _, err := os.Stat(recovery.Path(dir, "rep1") + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("atomic write left a .tmp file behind")
	}
	got, merr := recovery.Load(dir, "rep1")
	if merr != nil {
		t.Fatal(merr)
	}
	if !got.Has(1) || got.Has(2) {
		t.Fatalf("completed set wrong: %+v", got.Completed)
	}
	if merr := got.Validate("src", "cfg"); merr != nil {
		t.Fatalf("matching digests refused: %v", merr)
	}
}

func TestCheckpointRefusesChangedDigests(t *testing.T) {
	c := cp("rep1", 0)
	if merr := c.Validate("other", "cfg"); merr == nil || merr.Exit != model.ExitCheckpointMismatch {
		t.Fatalf("source change not refused with exit 4: %v", merr)
	}
	if merr := c.Validate("src", "other"); merr == nil || merr.Exit != model.ExitCheckpointMismatch {
		t.Fatalf("config change not refused with exit 4: %v", merr)
	}
}

func TestCorruptCheckpoint(t *testing.T) {
	dir := t.TempDir()
	if merr := recovery.Save(dir, cp("rep1")); merr != nil {
		t.Fatal(merr)
	}
	if err := os.WriteFile(recovery.Path(dir, "rep1"), []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, merr := recovery.Load(dir, "rep1"); merr == nil || merr.Exit != model.ExitCheckpointMismatch {
		t.Fatalf("corrupt checkpoint not refused with exit 4: %v", merr)
	}
}

// A crashed run resumes from its checkpoints: completed segments are
// not rewritten, the remaining ones finish, and the final state equals
// an uninterrupted run.
func TestResumeSkipsCompleted(t *testing.T) {
	dir := t.TempDir()
	var writes []int
	run := func(done map[int]bool, limit int) {
		for i := 0; i < 5 && len(done) < limit; i++ {
			if done[i] {
				continue
			}
			writes = append(writes, i)
			done[i] = true
			c := cp("rep1")
			for j := range done {
				if done[j] {
					c.Completed = append(c.Completed, segment.SegmentEntry{Index: j})
				}
			}
			if merr := recovery.Save(dir, c); merr != nil {
				t.Fatal(merr)
			}
		}
	}
	done := map[int]bool{}
	run(done, 2) // "crash" after two segments
	if len(writes) != 2 {
		t.Fatalf("first pass wrote %d segments, want 2", len(writes))
	}
	ck, merr := recovery.Load(dir, "rep1")
	if merr != nil {
		t.Fatal(merr)
	}
	if merr := ck.Validate("src", "cfg"); merr != nil {
		t.Fatal(merr)
	}
	for _, e := range ck.Completed {
		done[e.Index] = true
	}
	run(done, 5)
	if len(writes) != 5 {
		t.Fatalf("total writes = %d, want 5 (no rewrites)", len(writes))
	}
	for i, w := range writes {
		if w != i {
			t.Fatalf("writes = %v, want [0 1 2 3 4]", writes)
		}
	}
}
