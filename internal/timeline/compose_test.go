package timeline

import "testing"

func mkSamples(start, delta int64, n int) []SampleRef {
	var out []SampleRef
	for i := 0; i < n; i++ {
		out = append(out, SampleRef{
			Seq: i, RefStart: start + int64(i)*delta, SrcOffset: int64(i) * 100,
		})
	}
	return out
}

func TestComposeNonOverlap(t *testing.T) {
	tracks := []Track{
		{ID: "a", Priority: 0, Samples: mkSamples(0, 100, 3)},   // [0,300)
		{ID: "b", Priority: 1, Samples: mkSamples(300, 100, 3)}, // [300,600)
	}
	res := Compose(tracks, nil)
	if len(res.Frames) != 6 {
		t.Fatalf("frames = %d, want 6", len(res.Frames))
	}
	for i, f := range res.Frames {
		wantTrack := "a"
		if i >= 3 {
			wantTrack = "b"
		}
		if f.TrackID != wantTrack {
			t.Fatalf("frame %d owned by %s, want %s", i, f.TrackID, wantTrack)
		}
		if f.Index != i {
			t.Fatalf("frame index = %d, want %d", f.Index, i)
		}
	}
	// Boundary at 300 belongs to the interval starting there (track b).
	if res.Frames[3].RefStart != 300 || res.Frames[3].TrackID != "b" {
		t.Fatalf("boundary frame = %+v", res.Frames[3])
	}
}

func TestComposeOverlapPriority(t *testing.T) {
	tracks := []Track{
		{ID: "low", Priority: 0, Samples: mkSamples(0, 100, 10)},
		{ID: "high", Priority: 1, Samples: mkSamples(250, 100, 3)}, // covers [250,550)
	}
	res := Compose(tracks, nil)
	for _, f := range res.Frames {
		if f.RefStart >= 250 && f.RefStart < 550 {
			if f.TrackID != "high" {
				t.Fatalf("frame at %d owned by %s, want high", f.RefStart, f.TrackID)
			}
		} else if f.TrackID != "low" {
			t.Fatalf("frame at %d owned by %s, want low", f.RefStart, f.TrackID)
		}
	}
}

func TestComposeCounts(t *testing.T) {
	samples := []SampleRef{
		{Seq: 0, RefStart: 0},
		{Seq: 1, RefStart: 100},
		{Seq: 2, RefStart: 100}, // duplicate
		{Seq: 3, RefStart: 50},  // reversed
		{Seq: 4, RefStart: 200},
		{Seq: 5, RefStart: 700}, // gap of 500 with median 100 -> 4 missing
		{Seq: 6, RefStart: 800},
	}
	tracks := []Track{{ID: "t", Priority: 0, Samples: samples}}
	res := Compose(tracks, nil)
	rep := res.Reports[0]
	if rep.Duplicates != 1 || rep.Reversed != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Missing != 4 {
		t.Fatalf("missing = %d, want 4", rep.Missing)
	}
	if rep.Kept != 5 {
		t.Fatalf("kept = %d, want 5", rep.Kept)
	}
	// Dropped samples must not appear as frame owners.
	for _, f := range res.Frames {
		if f.SampleSeq == 2 || f.SampleSeq == 3 {
			t.Fatalf("isolated sample %d owns frame %+v", f.SampleSeq, f)
		}
	}
}

func TestComposeTraceable(t *testing.T) {
	tracks := []Track{
		{ID: "x", Priority: 0, Samples: mkSamples(0, 33, 5)},
		{ID: "y", Priority: 1, Samples: mkSamples(10, 40, 4)},
	}
	res := Compose(tracks, []bool{true, false})
	if len(res.Frames) == 0 {
		t.Fatal("no frames")
	}
	for _, f := range res.Frames {
		if f.TrackID == "" {
			t.Fatalf("frame %d has no track", f.Index)
		}
		if f.RefEnd < f.RefStart {
			t.Fatalf("frame %d has negative span", f.Index)
		}
	}
	if res.Reports[0].Aligned != true || res.Reports[1].Aligned != false {
		t.Fatalf("aligned flags = %+v", res.Reports)
	}
}

func TestComposeEmpty(t *testing.T) {
	res := Compose(nil, nil)
	if len(res.Frames) != 0 || len(res.Reports) != 0 {
		t.Fatalf("empty compose = %+v", res)
	}
}
