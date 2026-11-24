package index_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/index"
	"mediad/internal/model"
)

// refs is a FrameSource over a slice, used only in tests.
type refs struct {
	items []index.FrameRef
	i     int
}

func (r *refs) Next() (index.FrameRef, error) {
	if r.i >= len(r.items) {
		return index.FrameRef{}, io.EOF
	}
	f := r.items[r.i]
	r.i++
	return f, nil
}

func ref(no uint64, pts int64, status model.FrameStatus) index.FrameRef {
	content := model.DigestBytes([]byte{byte(no), byte(no >> 8)})
	return index.FrameRef{
		FrameNo: no, TrackID: 1, PTS: pts, Offset: int64(no) * 1000,
		Size: 100, Content: content, Status: status,
	}
}

func TestBuildStreamIndexesOnlyUsable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.idx")
	src := &refs{items: []index.FrameRef{
		ref(1, 0, model.FrameOK),
		ref(2, 90, model.FrameHashMismatch),
		ref(3, 180, model.FrameOK),
		ref(4, 270, model.FrameTruncatedTail),
		ref(5, 360, model.FrameOK),
	}}
	st, err := index.BuildStream(src, path)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if st.Entries != 3 || st.Skipped != 2 {
		t.Fatalf("stats %+v", st)
	}
	entries, dropped, err := index.Open(path)
	if err != nil || dropped != 0 {
		t.Fatalf("open: err=%v dropped=%d", err, dropped)
	}
	for _, e := range entries {
		if e.FrameNo == 2 || e.FrameNo == 4 {
			t.Fatalf("bad frame %d entered the index", e.FrameNo)
		}
	}
	if got := index.LookupByFrame(entries, 3); got == nil || got.PTS != 180 {
		t.Fatalf("lookup frame 3 = %+v", got)
	}
	if index.LookupByFrame(entries, 2) != nil {
		t.Fatal("lookup found a non-indexed bad frame")
	}
}

func TestBuildStreamRejectsNonMonotonicPTS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.idx")
	src := &refs{items: []index.FrameRef{
		ref(1, 90, model.FrameOK),
		ref(2, 45, model.FrameOK), // PTS went backwards
	}}
	if _, err := index.BuildStream(src, path); err == nil {
		t.Fatal("accepted non-monotonic PTS")
	}
}

func TestBuildStreamRejectsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.idx")
	if _, err := index.BuildStream(&refs{}, path); err == nil {
		t.Fatal("built an index with no usable frames")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed build left an index behind")
	}
}

func TestBuildStreamReplacesPartialIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.idx")
	good := &refs{items: []index.FrameRef{ref(1, 0, model.FrameOK), ref(2, 90, model.FrameOK)}}
	if _, err := index.BuildStream(good, path); err != nil {
		t.Fatal(err)
	}
	// Chop the file to simulate an interrupted writer.
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, raw[:len(raw)-5], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, dropped, err := index.Open(path); err != nil || dropped == 0 {
		t.Fatalf("partial index not detected: err=%v dropped=%d", err, dropped)
	}
	fresh := &refs{items: []index.FrameRef{ref(1, 0, model.FrameOK), ref(2, 90, model.FrameOK), ref(3, 180, model.FrameOK)}}
	if _, err := index.BuildStream(fresh, path); err != nil {
		t.Fatal(err)
	}
	if _, dropped, err := index.Open(path); err != nil || dropped != 0 {
		t.Fatalf("replaced index still partial: err=%v dropped=%d", err, dropped)
	}
}

func TestBuildStreamIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.idx")
	b := filepath.Join(dir, "b.idx")
	mk := func() *refs {
		return &refs{items: []index.FrameRef{ref(1, 0, model.FrameOK), ref(2, 90, model.FrameOK)}}
	}
	if _, err := index.BuildStream(mk(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := index.BuildStream(mk(), b); err != nil {
		t.Fatal(err)
	}
	ra, _ := os.ReadFile(a)
	rb, _ := os.ReadFile(b)
	if string(ra) != string(rb) {
		t.Fatal("identical input produced different index bytes")
	}
}
