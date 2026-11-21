package index

import (
	"testing"

	"mediad/internal/clock"
	"mediad/internal/model"
)

func at(n, d int64) clock.Rational {
	r, err := clock.New(n, d)
	if err != nil {
		panic(err)
	}
	return r
}

// track builds a 30fps track with the given GOP and damaged sample set.
func track(samples int, gop int, damaged map[int]bool, synthesised map[int]bool) model.Track {
	tr := model.Track{ID: 1, Kind: model.Video, Timescale: 30000, FrameRate: at(30, 1)}
	dur := at(1000, 30000)
	tr.Duration = at(int64(samples)*1000, 30000)
	for i := 0; i < samples; i++ {
		d := at(int64(i)*1000, 30000)
		tr.Samples = append(tr.Samples, model.Sample{
			Index: i, Offset: int64(i) * 100, Size: 100,
			DTS: d, PTS: d, Duration: dur,
			Keyframe: i%gop == 0, Damaged: damaged[i], Synthesised: synthesised[i],
		})
	}
	return tr
}

func TestRebuildIndexesOnlyIntactSyncSamples(t *testing.T) {
	tr := track(90, 30, map[int]bool{30: true}, map[int]bool{60: true})
	idx, err := Rebuild(tr)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 1 {
		t.Fatalf("entries=%d want 1 (only sample 0 is intact)", len(idx.Entries))
	}
	if idx.Skipped != 2 {
		t.Fatalf("skipped=%d want 2", idx.Skipped)
	}
	if idx.Entries[0].SampleIndex != 0 {
		t.Fatalf("entry=%+v", idx.Entries[0])
	}
}

func TestRebuildRejectsNonMonotonic(t *testing.T) {
	tr := track(10, 5, nil, nil)
	tr.Samples[3].DTS = tr.Samples[2].DTS
	if _, err := Rebuild(tr); err == nil {
		t.Fatal("expected non-monotonic rejection")
	}
}

func TestSeekAndVerify(t *testing.T) {
	tr := track(300, 30, nil, nil)
	idx, err := Rebuild(tr)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 10 {
		t.Fatalf("entries=%d want 10", len(idx.Entries))
	}
	// Every entry must point at a real sync sample.
	for _, e := range idx.Entries {
		s := tr.Samples[e.SampleIndex]
		if !s.Keyframe || s.DTS != e.DTS {
			t.Fatalf("entry %+v disagrees with sample", e)
		}
	}
	bad, msgs, err := Verify(tr, idx, 17)
	if err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("mismatches=%d: %v", bad, msgs)
	}

	res, err := idx.Seek(at(1700, 30000), mustEnd(tr))
	if err != nil {
		t.Fatal(err)
	}
	if res.Entry.SampleIndex > 51 {
		t.Fatalf("seek picked sample %d, after target", res.Entry.SampleIndex)
	}
}

func TestSeekOutOfRange(t *testing.T) {
	tr := track(60, 30, nil, nil)
	idx, _ := Rebuild(tr)
	if _, err := idx.Seek(at(-1, 30), mustEnd(tr)); err == nil {
		t.Fatal("expected error before start")
	}
	if _, err := idx.Seek(at(1000, 30), mustEnd(tr)); err == nil {
		t.Fatal("expected error past end")
	}
}

func TestVerifyDetectsTamperedEntry(t *testing.T) {
	tr := track(120, 30, nil, nil)
	idx, _ := Rebuild(tr)
	idx.Entries[1].Time = at(999, 1)
	bad, msgs, err := Verify(tr, idx, 9)
	if err != nil {
		t.Fatal(err)
	}
	if bad == 0 {
		t.Fatal("tampered entry was not detected")
	}
	_ = msgs
}

func mustEnd(tr model.Track) clock.Rational {
	e, err := tr.End()
	if err != nil {
		panic(err)
	}
	return e
}
