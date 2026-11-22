package checkpoint

import (
	"os"
	"testing"

	"mediad/internal/fault"
	"mediad/internal/model"
)

func TestAdvanceIdempotentAndNoDoubleCount(t *testing.T) {
	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	segs := []model.Segment{
		{ID: "s1", Status: model.StatusOK, ContentDigest: "h1"},
		{ID: "s2", Status: model.StatusOK, ContentDigest: "h2"},
	}
	st, err := c.Advance("input-A", segs)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Segments) != 2 || len(st.Done) != 2 {
		t.Fatalf("state = %+v", st)
	}
	// 重复运行：幂等，不新增片段，不重复计数。
	st2, err := c.Advance("input-A", segs)
	if err != nil {
		t.Fatal(err)
	}
	if len(st2.Segments) != 2 || len(st2.Done) != 2 {
		t.Fatalf("rerun double-counted: %+v", st2)
	}
	done, err := c.IsDone("input-A", "s1")
	if err != nil || !done {
		t.Fatalf("IsDone = %v, %v", done, err)
	}
	doneOther, _ := c.IsDone("input-B", "s1")
	if doneOther {
		t.Fatal("segment must not be done for a different input")
	}
}

func TestInputChangeInvalidatesSegments(t *testing.T) {
	c, _ := New(t.TempDir())
	if _, err := c.Advance("input-A", []model.Segment{
		{ID: "s1", Status: model.StatusOK},
	}); err != nil {
		t.Fatal(err)
	}
	st, err := c.Advance("input-B", []model.Segment{
		{ID: "s9", Status: model.StatusOK},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.InputID != "input-B" || len(st.Segments) != 1 || st.Segments[0].ID != "s9" {
		t.Fatalf("old segments must be invalidated: %+v", st)
	}
	if st.Done["s1"] {
		t.Fatal("stale done flag survived input change")
	}
}

func TestExplicitInvalidation(t *testing.T) {
	c, _ := New(t.TempDir())
	if _, err := c.Advance("input-A", []model.Segment{
		{ID: "s1", Status: model.StatusOK},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.InvalidateInput(); err != nil {
		t.Fatal(err)
	}
	st, err := c.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.InputID != "" || len(st.Segments) != 0 || len(st.Done) != 0 {
		t.Fatalf("invalidate failed: %+v", st)
	}
}

func TestLoadMissingReturnsEmpty(t *testing.T) {
	c, _ := New(t.TempDir())
	st, err := c.Load()
	if err != nil || st.Done == nil || len(st.Segments) != 0 {
		t.Fatalf("missing checkpoint must load empty: %+v %v", st, err)
	}
}

func TestCorruptCheckpointRecoverable(t *testing.T) {
	dir := t.TempDir()
	c, _ := New(dir)
	if err := os.WriteFile(c.path, []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := c.Load()
	if err == nil {
		t.Fatal("corrupt checkpoint must surface an error")
	}
	// 损坏的检查点属于可恢复故障，流水线可从头重建。
	if !fault.IsRecoverable(err) {
		t.Fatalf("corrupt checkpoint must be recoverable: %v", err)
	}
}
