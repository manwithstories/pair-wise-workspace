package index

import (
	"testing"

	"medialine/internal/fault"
	"medialine/internal/model"
)

func track(id int, pts []int64, keys map[int]bool) *model.Track {
	t := &model.Track{ID: id, Timescale: 90000}
	for i, p := range pts {
		t.Samples = append(t.Samples, model.Sample{Number: i + 1, DTSUS: p, PTSUS: p, DurationUS: 1000, Keyframe: keys[i+1]})
	}
	return t
}

func TestBuildIndexAndQueries(t *testing.T) {
	tr := track(1, []int64{0, 1000, 2000, 3000, 4000}, map[int]bool{1: true, 3: true, 5: true})
	idx, err := Build(tr)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Len() != 3 {
		t.Fatalf("len=%d", idx.Len())
	}
	if kf, ok := idx.First(); !ok || kf.PTSUS != 0 || kf.SampleNumber != 1 {
		t.Fatalf("first=%+v", kf)
	}
	if kf, ok := idx.Last(); !ok || kf.PTSUS != 4000 {
		t.Fatalf("last=%+v", kf)
	}

	if kf, ok := idx.Before(2500); !ok || kf.PTSUS != 2000 {
		t.Fatalf("before=%+v", kf)
	}
	if kf, ok := idx.Before(0); !ok || kf.PTSUS != 0 {
		t.Fatalf("constraint at 0 should find keyframe at 0: %+v %v", kf, ok)
	}
	if _, ok := idx.Before(-1); ok {
		t.Fatal("no keyframe before -1")
	}
	if kf, ok := idx.After(2000); !ok || kf.PTSUS != 2000 {
		t.Fatalf("after=%+v", kf)
	}
	if kf, ok := idx.After(1); !ok || kf.PTSUS != 2000 {
		t.Fatalf("after1=%+v", kf)
	}
	if _, ok := idx.After(5000); ok {
		t.Fatal("no keyframe after end")
	}
	if kf, ok := idx.Exact(3000); ok {
		t.Fatalf("3000 is not a keyframe: %+v", kf)
	}
	if _, ok := idx.Exact(4000); !ok {
		t.Fatal("4000 should be exact")
	}
	if _, ok := idx.At(3); ok {
		t.Fatal("out of range At should fail")
	}
	if _, ok := idx.At(-1); ok {
		t.Fatal("negative At should fail")
	}
}

func TestSnapBack(t *testing.T) {
	tr := track(1, []int64{0, 1000, 2000, 3000}, map[int]bool{1: true, 3: true})
	idx, _ := Build(tr)

	res := idx.SnapBack(1500)
	if !res.Found || !res.Snapped || res.SnapUS != 0 || res.DeltaUS != -1500 {
		t.Fatalf("snap 1500=%+v", res)
	}
	res = idx.SnapBack(1200)
	if res.SnapUS != 0 || res.DeltaUS != -1200 {
		t.Fatalf("snap 1200=%+v", res)
	}
	onKey := idx.SnapBack(2000)
	if onKey.Snapped || onKey.DeltaUS != 0 || onKey.SnapUS != 2000 {
		t.Fatalf("on-key=%+v", onKey)
	}
	before := idx.SnapBack(-10)
	if before.Found {
		t.Fatalf("negative should not find: %+v", before)
	}
}

func TestBuildRejectsUnsortedKeyframes(t *testing.T) {
	tr := &model.Track{ID: 7}
	tr.Samples = []model.Sample{
		{Number: 1, PTSUS: 5000, Keyframe: true},
		{Number: 2, PTSUS: 1000, Keyframe: true},
	}
	_, err := Build(tr)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindBackwardsTime || fe.TrackID != 7 {
		t.Fatalf("wrong fault: %v", err)
	}
	if _, err := Build(nil); err == nil {
		t.Fatal("expected nil-track error")
	}
}

func TestMultiIndexSnapAll(t *testing.T) {
	a := track(1, []int64{0, 2000, 4000}, map[int]bool{1: true, 2: true, 3: true})
	b := track(2, []int64{1000, 3000}, map[int]bool{1: true, 2: true})
	empty := track(3, []int64{500}, map[int]bool{})
	multi, err := NewMulti([]*model.Track{a, b, empty})
	if err != nil {
		t.Fatal(err)
	}
	if ids := multi.TrackIDs(); len(ids) != 3 || ids[0] != 1 || ids[2] != 3 {
		t.Fatalf("ids=%v", ids)
	}
	if _, ok := multi.Track(2); !ok {
		t.Fatal("track 2 missing")
	}
	res, missing := multi.SnapAll(2500, []int{1, 2, 3, 9})
	if len(missing) != 2 || missing[0] != 3 || missing[1] != 9 {
		t.Fatalf("missing=%v", missing)
	}
	if res[1].SnapUS != 2000 || res[2].SnapUS != 1000 {
		t.Fatalf("res=%v", res)
	}
}
