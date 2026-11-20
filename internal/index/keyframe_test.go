package index

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mediamend/mediamend/internal/fault"
)

// writeSegment emits a segment file in the format the segment writer uses.
func writeSegment(t *testing.T, dir, name, rep string, startTS uint64, firstSeq uint64, frames int) SegmentRef {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "PAIRFLOWSEG1 rep=%s 640x360 800000 frames=%d\n", rep, frames); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < frames; i++ {
		// 16 hex chars = ts, 8 hex = rtp seq, decimal seq, decimal offset.
		if _, err := fmt.Fprintf(f, "%016x %08x %d %d\n", startTS+uint64(i)*3000, i, firstSeq+uint64(i), i*100); err != nil {
			t.Fatal(err)
		}
	}
	return SegmentRef{File: name, Representation: rep, StartTS: startTS, FirstSeq: firstSeq, FrameCount: frames}
}

func TestKeyframeBuildQuery(t *testing.T) {
	dir := t.TempDir()
	var refs []SegmentRef
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("cam1-360p-%05d.seg", i)
		refs = append(refs, writeSegment(t, dir, name, "360p", uint64(i)*90000, uint64(i*30), 30))
	}
	idx, faults := BuildKeyframe("cam1", refs, dir)
	if len(faults) != 0 {
		t.Fatalf("faults=%v", faults)
	}
	if len(idx.Entries) != 4 {
		t.Fatalf("entries=%d want 4", len(idx.Entries))
	}
	if err := idx.Save(dir); err != nil {
		t.Fatal(err)
	}
	loaded, lf := Load(filepath.Join(dir, FileName))
	if len(lf) != 0 {
		t.Fatalf("load faults=%v", lf)
	}
	if len(loaded.Entries) != 4 {
		t.Fatalf("loaded entries=%d", len(loaded.Entries))
	}

	// Query each exact keyframe time and in-between times.
	cases := []struct {
		ts   uint64
		want int
	}{
		{0, 0}, {1, 0}, {89999, 0}, {90000, 1}, {180000, 2}, {270000, 3}, {999999, 3},
	}
	for _, c := range cases {
		e, err := loaded.Query("360p", c.ts, dir)
		if err != nil {
			t.Fatalf("query ts=%d: %v", c.ts, err)
		}
		if e.MasterTS != uint64(c.want)*90000 {
			t.Fatalf("query ts=%d got %d want %d", c.ts, e.MasterTS, uint64(c.want)*90000)
		}
		// The entry must point at a real file and frame line.
		path := filepath.Join(dir, e.Segment)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if e.ByteOffset < 0 || e.ByteOffset >= info.Size() {
			t.Fatalf("byte offset %d outside %s", e.ByteOffset, e.Segment)
		}
	}
	if _, err := loaded.Query("720p", 0, dir); err == nil {
		t.Fatal("query for absent representation succeeded")
	}
	if _, err := loaded.Query("360p", 90000, dir); err != nil {
		t.Fatal(err)
	}
}

func TestKeyframeCorruptIndexRebuild(t *testing.T) {
	dir := t.TempDir()
	var refs []SegmentRef
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("cam1-360p-%05d.seg", i)
		refs = append(refs, writeSegment(t, dir, name, "360p", uint64(i)*90000, uint64(i*30), 30))
	}
	orig, _ := BuildKeyframe("cam1", refs, dir)
	if err := orig.Save(dir); err != nil {
		t.Fatal(err)
	}
	// Corrupt the index: make a timestamp go backwards.
	bad := *orig
	bad.Entries = append([]KeyframeEntry(nil), orig.Entries...)
	bad.Entries[1].MasterTS = 0
	if err := bad.Save(dir); err != nil {
		t.Fatal(err)
	}
	_, faults := Load(filepath.Join(dir, FileName))
	if len(faults) == 0 || !faults.Fatal() {
		t.Fatalf("corrupt index not rejected: faults=%v", faults)
	}
	if faults.Count(fault.KindIndexCorrupt) == 0 {
		t.Fatalf("faults=%v", faults)
	}
	// Rebuild from the segment files.
	rebuilt, rf := Rebuild("cam1", dir)
	if len(rebuilt.Entries) != 3 {
		t.Fatalf("rebuilt entries=%d want 3 (faults=%v)", len(rebuilt.Entries), rf)
	}
	for i := 1; i < len(rebuilt.Entries); i++ {
		if rebuilt.Entries[i].MasterTS <= rebuilt.Entries[i-1].MasterTS {
			t.Fatalf("rebuilt index not monotonic at %d", i)
		}
	}
	if f := rebuilt.CheckIntegrity(dir); len(f) != 0 {
		t.Fatalf("rebuilt index does not resolve: %v", f)
	}
}

func TestKeyframeQueryDetectsMissingSegment(t *testing.T) {
	dir := t.TempDir()
	refs := []SegmentRef{writeSegment(t, dir, "cam1-360p-00000.seg", "360p", 0, 0, 10)}
	idx, _ := BuildKeyframe("cam1", refs, dir)
	if err := os.Remove(filepath.Join(dir, "cam1-360p-00000.seg")); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Query("360p", 0, dir); err == nil {
		t.Fatal("query resolved a missing segment")
	}
	faults := idx.CheckIntegrity(dir)
	if len(faults) == 0 || faults[0].Kind != fault.KindIndexBounds {
		t.Fatalf("integrity faults=%v", faults)
	}
}

func TestKeyframeLoadRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, faults := Load(path); len(faults) == 0 || !faults.Fatal() {
		t.Fatalf("garbage index accepted: %v", faults)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"camera":"c","entries":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, faults := Load(path); len(faults) == 0 {
		t.Fatal("empty index accepted")
	}
}
