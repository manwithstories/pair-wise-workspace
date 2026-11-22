package timeline

import (
	"testing"

	"mediacore/internal/clock"
	"mediacore/internal/manifest"
)

func playlist() *manifest.Playlist {
	return &manifest.Playlist{Format: "hls", Tracks: []manifest.Track{
		{TrackMeta: manifest.TrackMeta{ID: "v1", Kind: "video", Codec: "avc1", Timescale: 90000},
			Segments: []manifest.Segment{
				{Index: 0, URI: "v1.m4s", Dur: clock.Must(2, 1)},
				{Index: 1, URI: "v2.m4s", Dur: clock.Must(2, 1)},
			}},
		{TrackMeta: manifest.TrackMeta{ID: "a1", Kind: "audio", Codec: "mp4a", Timescale: 48000},
			Segments: []manifest.Segment{
				{Index: 0, URI: "a1.m4s", Dur: clock.Must(2, 1)},
				{Index: 1, URI: "", Dur: clock.Must(1, 2), Gap: true},
			}},
	}}
}

func TestBuildAssignsStarts(t *testing.T) {
	tl, err := Build(playlist())
	if err != nil {
		t.Fatal(err)
	}
	v := tl.Track("v1")
	if v.Segments[1].Start != clock.Must(2, 1) || v.Segments[1].End != clock.Must(4, 1) {
		t.Fatalf("bad video placement: %+v", v.Segments[1])
	}
	a := tl.Track("a1")
	if !a.Segments[1].Gap || a.Segments[1].End != clock.Must(5, 2) {
		t.Fatalf("bad gap placement: %+v", a.Segments[1])
	}
}

func TestAlignmentMs(t *testing.T) {
	tl1, _ := Build(playlist())
	pl2 := playlist()
	pl2.Tracks[0].Segments[0].Dur = clock.Must(2, 1)
	tl2, _ := Build(pl2)
	if got := AlignmentMs(tl1, tl2); got != 0 {
		t.Fatalf("alignment = %v, want 0", got)
	}
	// Shift every video segment of tl2 by 30ms.
	shifted := &Timeline{Tracks: []TrackTimeline{{
		Meta: tl2.Tracks[0].Meta,
	}}}
	for _, s := range tl2.Tracks[0].Segments {
		d := clock.Must(3, 100)
		s.Start, _ = s.Start.Add(d)
		s.End, _ = s.End.Add(d)
		shifted.Tracks[0].Segments = append(shifted.Tracks[0].Segments, s)
	}
	if got := AlignmentMs(tl1, shifted); got != 30 {
		t.Fatalf("alignment = %v, want 30", got)
	}
}

func TestTopoSort(t *testing.T) {
	order, err := TopoSort([]Node{
		{Name: "cut", Deps: []string{"parse", "index"}},
		{Name: "parse"},
		{Name: "index", Deps: []string{"parse"}},
		{Name: "report", Deps: []string{"cut"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"parse", "index", "cut", "report"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestTopoSortCycle(t *testing.T) {
	_, err := TopoSort([]Node{
		{Name: "a", Deps: []string{"b"}},
		{Name: "b", Deps: []string{"a"}},
	})
	if err == nil {
		t.Fatal("cycle should be an error")
	}
}

func TestTopoSortUnknownDep(t *testing.T) {
	_, err := TopoSort([]Node{{Name: "a", Deps: []string{"ghost"}}})
	if err == nil {
		t.Fatal("unknown dependency should be an error")
	}
}
