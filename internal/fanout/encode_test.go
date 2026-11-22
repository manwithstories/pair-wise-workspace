package fanout

import (
	"os"
	"testing"

	"mediatool/internal/clock"
	"mediatool/internal/fixture"
	"mediatool/internal/repair"
	"mediatool/internal/ts"
)

func buildTimeline(durMs int) repair.Timeline {
	s := fixture.Generate(7, durMs, fixture.FaultSpec{})
	res := ts.Parse(s.Data)
	return repair.Recover(res, fixture.PIDVideo, 2000, nil)
}

func testReps() []Representation {
	return []Representation{
		{ID: 0, Bitrate: 800_000, KeyInterval: clock.MsToTicks(2000)},
		{ID: 1, Bitrate: 400_000, KeyInterval: clock.MsToTicks(1000)},
		{ID: 2, Bitrate: 200_000, KeyInterval: clock.MsToTicks(500)},
	}
}

func TestEncodeDeterministic(t *testing.T) {
	tl := buildTimeline(6000)
	dir1, dir2 := t.TempDir(), t.TempDir()
	e1 := &Encoder{Dir: dir1, Seed: 42}
	e2 := &Encoder{Dir: dir2, Seed: 42}
	f1, k1, err := e1.Encode(testReps(), tl)
	if err != nil {
		t.Fatal(err)
	}
	f2, k2, err := e2.Encode(testReps(), tl)
	if err != nil {
		t.Fatal(err)
	}
	if len(f1) != len(f2) || len(k1) != len(k2) {
		t.Fatal("structure differs between runs")
	}
	for i := range f1 {
		if f1[i].SHA256 != f2[i].SHA256 {
			t.Fatalf("segment %d hash differs: %s vs %s", i, f1[i].SHA256, f2[i].SHA256)
		}
	}
	// 每段都有关键帧索引项，且指向文件内。
	for _, k := range k1 {
		st, err := os.Stat(k.Path)
		if err != nil {
			t.Fatal(err)
		}
		if k.Offset < 0 || k.Offset+int64(k.Size) > st.Size() {
			t.Fatalf("key entry out of range: %+v size=%d", k, st.Size())
		}
	}
}

func TestEncodeKeyframePerSegment(t *testing.T) {
	tl := buildTimeline(6000)
	e := &Encoder{Dir: t.TempDir(), Seed: 1}
	_, keys, err := e.Encode(testReps(), tl)
	if err != nil {
		t.Fatal(err)
	}
	// 每个 (rep, seg) 至少一个关键帧（片段首帧强制为关键帧）。
	got := map[[2]int]int{}
	for _, k := range keys {
		got[[2]int{k.RepID, k.SegID}]++
	}
	for _, rep := range testReps() {
		for _, seg := range tl.Segments {
			if got[[2]int{rep.ID, seg.ID}] == 0 {
				t.Fatalf("rep %d seg %d has no keyframe", rep.ID, seg.ID)
			}
		}
	}
}

func TestEncodeLossKeepsKeyframes(t *testing.T) {
	tl := buildTimeline(6000)
	e := &Encoder{Dir: t.TempDir(), Seed: 3, LossRate: 0.5}
	files, keys, err := e.Encode(testReps(), tl)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 || len(keys) == 0 {
		t.Fatal("no output")
	}
	// 丢包只丢非关键帧：每段仍至少一个关键帧。
	got := map[[2]int]bool{}
	for _, k := range keys {
		got[[2]int{k.RepID, k.SegID}] = true
	}
	for _, f := range files {
		if !got[[2]int{f.RepID, f.SegID}] {
			t.Fatalf("rep %d seg %d lost all keyframes", f.RepID, f.SegID)
		}
	}
}
