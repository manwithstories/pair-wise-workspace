package segment

import (
	"errors"
	"testing"

	"mediatime/internal/clock"
)

func TestExtractFragmentValid(t *testing.T) {
	data := buildFMP4(fmp4Spec{
		trackID:   1,
		baseDTS:   90_000,
		durations: []uint32{30_000, 30_000, 30_000},
		sizes:     []uint32{100, 120, 80},
		nonSync:   []bool{false, true, true},
	})
	f, err := ExtractFragment(7, "seg7.m4s", data)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	if f.Sequence != 7 || len(f.Samples) != 3 {
		t.Fatalf("unexpected fragment: %+v", f)
	}
	// DTS 应按 baseDTS + 累计 duration 推进。
	wantDTS := []uint64{90_000, 120_000, 150_000}
	for i, s := range f.Samples {
		if s.RawDTS != wantDTS[i] {
			t.Fatalf("sample %d dts=%d want %d", i, s.RawDTS, wantDTS[i])
		}
		if s.Offset < 0 || s.Offset+int64(s.Size) > f.FileSize {
			t.Fatalf("sample %d offset out of bounds", i)
		}
	}
	// 仅第一个样本是同步点。
	if f.Samples[0].IsSync != true || f.Samples[1].IsSync != false {
		t.Fatal("sync flag interpretation wrong")
	}
	// 偏移应严格单调且相邻偏移差等于前一样本大小。
	for i := 1; i < len(f.Samples); i++ {
		if f.Samples[i].Offset <= f.Samples[i-1].Offset {
			t.Fatal("offset not monotone")
		}
		if f.Samples[i].Offset != f.Samples[i-1].Offset+int64(f.Samples[i-1].Size) {
			t.Fatal("offset gaps do not match sample sizes")
		}
	}
}

func TestExtractFragmentTruncated(t *testing.T) {
	data := buildFMP4(fmp4Spec{
		trackID: 1, baseDTS: 0,
		durations: []uint32{30_000}, sizes: []uint32{500}, truncate: 20,
	})
	_, err := ExtractFragment(0, "t.m4s", data)
	// 截断点可能表现为盒头截断或声明越界，两者都属于损坏盒，且不得产出索引。
	if !errors.Is(err, ErrTruncatedBox) && !errors.Is(err, ErrBoxOverflow) {
		t.Fatalf("want truncated/overflow box error, got %v", err)
	}
	var fe *FragmentError
	if !errors.As(err, &fe) {
		t.Fatalf("error must carry fragment location, got %v", err)
	}
	if fe.Sequence != 0 {
		t.Fatalf("error must carry sequence, got %+v", fe)
	}
}

func TestExtractFragmentCorruptBoxOverflow(t *testing.T) {
	data := buildFMP4(fmp4Spec{
		trackID: 1, baseDTS: 0,
		durations: []uint32{30_000}, sizes: []uint32{100}, corruptBox: true,
	})
	if _, err := ExtractFragment(1, "c.m4s", data); !errors.Is(err, ErrBoxOverflow) {
		t.Fatalf("want ErrBoxOverflow, got %v", err)
	}
}

func TestExtractFragmentMissingBoxes(t *testing.T) {
	noMdat := buildFMP4(fmp4Spec{
		trackID: 1, baseDTS: 0, durations: []uint32{1}, sizes: []uint32{1}, omitMdat: true,
	})
	if _, err := ExtractFragment(0, "x", noMdat); !errors.Is(err, ErrNoMdat) {
		t.Fatalf("want ErrNoMdat, got %v", err)
	}
	noMoof := buildFMP4(fmp4Spec{
		trackID: 1, baseDTS: 0, durations: []uint32{1}, sizes: []uint32{1}, omitMoof: true,
	})
	if _, err := ExtractFragment(0, "x", noMoof); !errors.Is(err, ErrNoMoof) {
		t.Fatalf("want ErrNoMoof, got %v", err)
	}
}

// 独立核验：重新读取 BuildIndex 的输出，逐条验证偏移与时间单调性。
func TestBuildIndexIndependentVerification(t *testing.T) {
	frag := buildTwoFragments(t)
	tl := buildTimelineFor(t, frag)
	idx, err := BuildIndex(frag, tl)
	if err != nil {
		t.Fatalf("build index failed: %v", err)
	}
	if idx.Keys != len(idx.Entries) || idx.Keys == 0 {
		t.Fatal("index key count wrong")
	}
	fileSizes := map[int]int64{}
	for _, f := range frag {
		fileSizes[f.Sequence] = f.FileSize
	}
	var prevTime clock.Tick = -1
	var prevOffset int64 = -1
	var prevSeq int = -1
	for _, e := range idx.Entries {
		// 重新核验文件边界。
		if e.Offset < 0 || e.Offset+int64(e.Size) > fileSizes[e.Sequence] {
			t.Fatalf("entry offset outside file: %+v", e)
		}
		if e.Time <= prevTime {
			t.Fatalf("time not strictly monotone: %+v", e)
		}
		if e.Sequence == prevSeq && e.Offset <= prevOffset {
			t.Fatalf("offset not monotone within fragment: %+v", e)
		}
		prevTime, prevOffset, prevSeq = e.Time, e.Offset, e.Sequence
	}
}

func TestBuildIndexDeterministicHash(t *testing.T) {
	frag := buildTwoFragments(t)
	tl := buildTimelineFor(t, frag)
	idx1, err := BuildIndex(frag, tl)
	if err != nil {
		t.Fatal(err)
	}
	idx2, err := BuildIndex(frag, tl)
	if err != nil {
		t.Fatal(err)
	}
	if idx1.SHA256 != idx2.SHA256 || len(idx1.SHA256) != 64 {
		t.Fatalf("index hash unstable or wrong length: %q vs %q", idx1.SHA256, idx2.SHA256)
	}
}

func TestBuildIndexRejectsOutOfBounds(t *testing.T) {
	data := buildFMP4(fmp4Spec{
		trackID: 1, baseDTS: 0, durations: []uint32{30_000}, sizes: []uint32{64},
	})
	f, err := ExtractFragment(0, "a.m4s", data)
	if err != nil {
		t.Fatal(err)
	}
	// 人为破坏偏移到文件外。
	f.Samples[0].Offset = f.FileSize + 10
	tl := buildTimelineFor(t, []*ExtractedFragment{f})
	if _, err := BuildIndex([]*ExtractedFragment{f}, tl); !errors.Is(err, ErrBoxOverflow) {
		t.Fatalf("want ErrBoxOverflow, got %v", err)
	}
}

func buildTwoFragments(t *testing.T) []*ExtractedFragment {
	t.Helper()
	specs := []fmp4Spec{
		{trackID: 1, baseDTS: 0, durations: []uint32{30_000, 30_000}, sizes: []uint32{80, 90}, nonSync: []bool{false, true}},
		{trackID: 1, baseDTS: 60_000, durations: []uint32{30_000, 30_000}, sizes: []uint32{70, 85}, nonSync: []bool{false, true}},
	}
	var out []*ExtractedFragment
	for i, sp := range specs {
		f, err := ExtractFragment(i, uriFor(i), buildFMP4(sp))
		if err != nil {
			t.Fatalf("fragment %d: %v", i, err)
		}
		out = append(out, f)
	}
	return out
}

func uriFor(i int) string {
	return "seg" + string(rune('0'+i)) + ".m4s"
}

func buildTimelineFor(t *testing.T, frags []*ExtractedFragment) *clock.Timeline {
	t.Helper()
	var samples []clock.Sample
	expected := map[int]clock.Tick{}
	for _, f := range frags {
		expected[f.Sequence] = clock.Tick(f.Sequence) * 60_000
		samples = append(samples, f.ToClockSamples()...)
	}
	tl, err := clock.RepairSamples(samples, clock.RepairConfig{
		Window: 4, OutlierTick: 9_000, Expected: expected,
	}, nil)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	return tl
}
