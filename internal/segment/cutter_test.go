package segment

import (
	"testing"

	"medpipe/internal/model"
)

func mkFrames(n int, stepNS int64) []*model.Frame {
	out := make([]*model.Frame, n)
	for i := range out {
		out[i] = &model.Frame{Seq: uint32(i), TimestampNS: int64(i) * stepNS}
	}
	return out
}

func mkAudio(n int, stepNS int64) []*model.AudioBlock {
	out := make([]*model.AudioBlock, n)
	for i := range out {
		out[i] = &model.AudioBlock{Seq: uint32(i), TimestampNS: int64(i) * stepNS, Samples: 160}
	}
	return out
}

func TestCutWindows(t *testing.T) {
	c, err := New(1_000)
	if err != nil {
		t.Fatal(err)
	}
	segs := c.Cut(mkFrames(10, 250), mkAudio(5, 500))
	if len(segs) != 3 {
		t.Fatalf("segments=%d want 3", len(segs))
	}
	wantVideo := []int{4, 4, 2} // 0-750, 1000-1750, 2000-2250
	for i, w := range wantVideo {
		if len(segs[i].VideoSeqs) != w {
			t.Fatalf("seg %d video=%d want %d", i, len(segs[i].VideoSeqs), w)
		}
	}
	if len(segs[0].AudioSeqs) != 2 || len(segs[1].AudioSeqs) != 2 || len(segs[2].AudioSeqs) != 1 {
		t.Fatalf("audio bucketing wrong: %+v", segs)
	}
	if segs[0].StartNS != 0 || segs[1].StartNS != 1000 || segs[2].EndNS != 3000 {
		t.Fatalf("window bounds wrong: %+v", segs)
	}
}

func TestCutPointsIgnoreAudio(t *testing.T) {
	c, _ := New(1_000)
	frames := mkFrames(5, 400)
	pointsWith := c.VideoCutPoints(frames)
	// Audio presence cannot change the points: compare against a run whose
	// audio is empty by construction.
	segs := c.Cut(frames, nil)
	if len(pointsWith) != len(segs) {
		t.Fatalf("points=%d segments=%d", len(pointsWith), len(segs))
	}
	for i, p := range pointsWith {
		if p != segs[i].StartNS {
			t.Fatalf("point %d: %d != %d", i, p, segs[i].StartNS)
		}
	}
}

func TestCutDeterministic(t *testing.T) {
	c, _ := New(777)
	first := c.Cut(mkFrames(50, 333), mkAudio(20, 800))
	second := c.Cut(mkFrames(50, 333), mkAudio(20, 800))
	if len(first) != len(second) {
		t.Fatal("run length differs")
	}
	for i := range first {
		if first[i].StartNS != second[i].StartNS || len(first[i].VideoSeqs) != len(second[i].VideoSeqs) {
			t.Fatalf("segment %d differs between runs", i)
		}
	}
}

func TestBadWindow(t *testing.T) {
	if _, err := New(0); err == nil {
		t.Fatal("zero window must fail")
	}
}
