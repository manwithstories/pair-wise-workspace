package index

import (
	"os"
	"path/filepath"
	"testing"

	"medcheck/internal/media"
)

func testAUs() []media.AccessUnit {
	return []media.AccessUnit{
		{Index: 0, GOPID: 0, PTSMS: 0, KeyFrame: true},
		{Index: 1, GOPID: 0, PTSMS: 40},
		{Index: 2, GOPID: 1, PTSMS: 80, KeyFrame: true},
		{Index: 3, GOPID: 1, PTSMS: 120},
	}
}

func TestBuildGOPKeyframeIndex(t *testing.T) {
	idx, errs := BuildGOP(testAUs(), nil, 17)
	if len(errs) != 0 {
		t.Fatalf("unexpected GOP errors: %v", errs)
	}
	if len(idx.Segments) != 2 || idx.KeyframesFound != 2 || idx.KeyframesExpected != 2 {
		t.Fatalf("bad index: %+v", idx)
	}
	if idx.Segments[0].StartMS != 0 || idx.Segments[1].StartMS != 80 {
		t.Fatalf("segment times wrong: %+v", idx.Segments)
	}
}

func TestAtomicSaveResumeAndStableHashes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "index.json")
	build := func() (*Index, error) {
		idx, _ := BuildGOP(testAUs(), map[int]string{0: "a", 1: "b"}, 17)
		// BuildGOP currently falls back only for empty hashes; set valid hashes.
		idx.Segments[0].Hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		idx.Segments[1].Hash = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
		return idx, nil
	}
	first, resumed, err := ResumeOrBuild(path, build)
	if err != nil || resumed {
		t.Fatalf("first build resumed=%v err=%v", resumed, err)
	}
	second, resumed, err := ResumeOrBuild(path, build)
	if err != nil || !resumed {
		t.Fatalf("second load resumed=%v err=%v", resumed, err)
	}
	if first.Segments[0].Hash != second.Segments[0].Hash {
		t.Fatal("segment hash changed across resume")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file should be renamed, stat err=%v", err)
	}
}
