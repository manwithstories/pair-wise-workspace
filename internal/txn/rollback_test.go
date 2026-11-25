package txn

import (
	"os"
	"testing"
)

func TestDiscardStagingRemovesHalfFragmentsOnly(t *testing.T) {
	c, f := NewCommitter(t.TempDir())
	if f != nil {
		t.Fatal(f)
	}
	defer c.Close()

	if _, f := c.CommitSegment(0, []byte("zero")); f != nil {
		t.Fatal(f)
	}
	if _, f := c.PrepareSegment(1, []byte("one-staged")); f != nil {
		t.Fatal(f)
	}
	if !c.IsCommitted(0) {
		t.Fatal("segment 0 should be committed")
	}
	if c.IsCommitted(1) {
		t.Fatal("segment 1 must not be committed yet")
	}

	n, f := c.DiscardStaging()
	if f != nil {
		t.Fatal(f)
	}
	if n != 1 {
		t.Fatalf("removed=%d want 1", n)
	}
	if _, err := os.ReadFile(c.tmpPath(1)); err == nil {
		t.Fatal("staging file should be discarded")
	}
	if !c.IsCommitted(0) {
		t.Fatal("committed fragment must survive staging discard")
	}

	// Discard is idempotent when nothing is staged.
	if n, f := c.DiscardStaging(); f != nil || n != 0 {
		t.Fatalf("second discard n=%d f=%v want 0,nil", n, f)
	}
}
