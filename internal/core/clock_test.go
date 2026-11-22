package core

import (
	"testing"

	"mediacheck/internal/model"
)

func frag(seq uint32, tfdt, dur int64, sha string) model.Fragment {
	start := model.FromTimescale(tfdt, 48000)
	end := model.FromTimescale(tfdt+dur, 48000)
	return model.Fragment{
		Sequence: seq, Timescale: 48000, BaseMediaDecodeTime: tfdt, ContentSHA: sha, RawSize: 10,
		Samples:   []model.Sample{{TrackID: 1, Duration: dur, Data: []byte{1}}},
		Intervals: []model.Interval{{Start: start, End: end, FragmentSeq: seq}},
	}
}

func TestNormalizeDetectsGapAndBuildsTimeline(t *testing.T) {
	in := []model.Fragment{frag(3, 960, 960, "c"), frag(1, 0, 960, "a")}
	rep, err := NormalizeTimeline(in, RegressionReject)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ordered[0].Sequence != 1 || rep.Ordered[1].Sequence != 3 {
		t.Fatal("fragments not sorted by seq")
	}
	if len(rep.Gaps) != 1 || rep.Gaps[0].MissingFrom != 2 || rep.Gaps[0].MissingTo != 2 {
		t.Fatalf("gaps = %+v", rep.Gaps)
	}
	if len(rep.Timeline) != 2 {
		t.Fatalf("timeline = %d", len(rep.Timeline))
	}
	// 入参未被修改。
	if in[0].Sequence != 3 {
		t.Fatal("input must not be mutated")
	}
}

func TestTFDTRegressionRejected(t *testing.T) {
	in := []model.Fragment{frag(1, 0, 960, "a"), frag(2, 100, 960, "b")}
	_, err := NormalizeTimeline(in, RegressionReject)
	me, ok := model.AsMediaError(err)
	if !ok || me.Category != model.ErrTFDTRegression {
		t.Fatalf("want tfdt_regression, got %v", err)
	}
	if me.Source == nil || me.Source.FragmentSeq != 2 {
		t.Fatalf("error must carry source interval: %+v", me.Source)
	}
}

func TestTFDTRegressionRebase(t *testing.T) {
	in := []model.Fragment{frag(1, 0, 960, "a"), frag(2, 100, 960, "b")}
	rep, err := NormalizeTimeline(in, RegressionRebase)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rebases) != 1 || rep.Rebases[0].FragmentSeq != 2 {
		t.Fatalf("rebases = %+v", rep.Rebases)
	}
	// 重排后第二段起点必须等于第一段结束，时间线连续无回退。
	firstEnd := rep.Timeline[0].End
	secondStart := rep.Timeline[1].Start
	if firstEnd.Cmp(secondStart) != 0 {
		t.Fatalf("rebased timeline not continuous: %v vs %v", firstEnd, secondStart)
	}
}

func TestDuplicateDedupAndConflict(t *testing.T) {
	// 同序号同摘要 → 去重。
	in := []model.Fragment{frag(1, 0, 960, "a"), frag(1, 0, 960, "a"), frag(2, 960, 960, "b")}
	rep, err := NormalizeTimeline(in, RegressionReject)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Ordered) != 2 || len(rep.Duplicates) != 1 || rep.Duplicates[0] != 1 {
		t.Fatalf("dedup = %+v dup=%v", rep.Ordered, rep.Duplicates)
	}
	// 同序号不同摘要 → 整组失败。
	bad := []model.Fragment{frag(1, 0, 960, "a"), frag(1, 0, 960, "zz")}
	_, err = NormalizeTimeline(bad, RegressionReject)
	me, ok := model.AsMediaError(err)
	if !ok || me.Category != model.ErrDuplicateConflict {
		t.Fatalf("want duplicate_conflict, got %v", err)
	}
}

func TestEmptyInputRejected(t *testing.T) {
	if _, err := NormalizeTimeline(nil, RegressionReject); err == nil {
		t.Fatal("empty fragment set must be rejected")
	}
}
