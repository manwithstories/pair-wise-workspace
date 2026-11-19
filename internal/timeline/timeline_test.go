package timeline

import (
	"testing"

	"pairflow/media/internal/clock"
	"pairflow/media/internal/mp4"
	"pairflow/media/internal/subtitle"
)

func cueTrack(name string, prio int, cues ...subtitle.Cue) TrackInput {
	return TrackInput{Name: name, Kind: KindSubtitle, Priority: prio,
		Sub: &subtitle.Document{Format: "srt", Track: name, Cues: cues}}
}

func cue(start, end int64, text string, off int64) subtitle.Cue {
	return subtitle.Cue{Start: clock.NewRational(start, 1), End: clock.NewRational(end, 1),
		Text: text, Offset: off}
}

func TestComposeOrdersEntries(t *testing.T) {
	doc := cueTrack("sub", 0,
		cue(2, 3, "second", 10),
		cue(0, 1, "first", 0),
		cue(1, 2, "middle", 5),
	)
	tl := Compose([]TrackInput{doc}, Options{
		FrameDuration: clock.NewRational(1, 25),
	})
	if tl.SubtitleCount != 3 {
		t.Fatalf("count = %d", tl.SubtitleCount)
	}
	want := []string{"first", "middle", "second"}
	for i, w := range want {
		if tl.Entries[i].Text != w {
			t.Fatalf("entry %d = %q, want %q", i, tl.Entries[i].Text, w)
		}
	}
	if len(tl.Errors) != 0 {
		t.Fatalf("errors: %v", tl.Errors)
	}
}

func TestConflictPolicies(t *testing.T) {
	base := []TrackInput{cueTrack("sub", 0,
		cue(0, 5, "early", 0),
		cue(3, 8, "late", 20),
	)}
	lastWins := Compose(base, Options{Policy: ConflictLastWins, FrameDuration: clock.NewRational(1, 25)})
	if len(lastWins.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v", lastWins.Conflicts)
	}
	if lastWins.Conflicts[0].Overlap != clock.NewRational(2, 1) {
		t.Fatalf("overlap = %s", lastWins.Conflicts[0].Overlap)
	}
	if !lastWins.Entries[0].Trimmed || lastWins.Entries[0].End != clock.NewRational(3, 1) {
		t.Fatalf("earlier cue not trimmed: %+v", lastWins.Entries[0])
	}

	keep := Compose(base, Options{Policy: ConflictKeepBoth, FrameDuration: clock.NewRational(1, 25)})
	if len(keep.Entries) != 2 || keep.Entries[0].End != clock.NewRational(5, 1) {
		t.Fatalf("keep-both changed cues: %+v", keep.Entries)
	}
	if keep.Conflicts[0].Resolved != "kept-both" {
		t.Fatalf("resolution = %s", keep.Conflicts[0].Resolved)
	}

	reject := Compose(base, Options{Policy: ConflictReject, FrameDuration: clock.NewRational(1, 25)})
	if len(reject.Entries) != 1 || reject.Entries[0].Text != "early" {
		t.Fatalf("reject kept wrong cue: %+v", reject.Entries)
	}
}

func TestWindowClipsAndDrops(t *testing.T) {
	doc := cueTrack("sub", 0,
		cue(0, 2, "before", 0),
		cue(2, 6, "across", 10),
		cue(8, 9, "after", 20),
	)
	tl := Compose([]TrackInput{doc}, Options{
		Window:        Window{Start: clock.NewRational(3, 1), End: clock.NewRational(7, 1)},
		FrameDuration: clock.NewRational(1, 25),
	})
	if len(tl.Entries) != 1 {
		t.Fatalf("entries = %+v", tl.Entries)
	}
	e := tl.Entries[0]
	if e.Text != "across" || e.Start != clock.NewRational(3, 1) || e.End != clock.NewRational(6, 1) {
		t.Fatalf("clipped entry = %+v", e)
	}
}

func TestMediaEntriesCarrySamples(t *testing.T) {
	tr := &mp4.TrackIndex{
		ID: 1, Handler: "vide", Timescale: 30000,
		Samples: []mp4.Sample{
			{Number: 1, Offset: 100, Size: 10, DTS: clock.Zero(), PTS: clock.Zero(), Sync: true},
			{Number: 2, Offset: 110, Size: 10, DTS: clock.NewRational(1, 30), PTS: clock.NewRational(1, 30)},
		},
	}
	tl := Compose([]TrackInput{{Name: "video", Kind: KindVideo, MP4: tr}}, Options{
		FrameDuration: clock.NewRational(1, 30),
	})
	if tl.VideoCount != 2 {
		t.Fatalf("video count = %d", tl.VideoCount)
	}
	if tl.Entries[0].End != clock.NewRational(1, 30) {
		t.Fatalf("first sample end = %s", tl.Entries[0].End)
	}
	if tl.Entries[1].Offset != 110 {
		t.Fatalf("second sample offset = %d", tl.Entries[1].Offset)
	}
}

func TestSubtitleBounds(t *testing.T) {
	tl := Compose([]TrackInput{cueTrack("sub", 0,
		cue(1, 2, "a", 0),
		cue(5, 7, "b", 10),
	)}, Options{FrameDuration: clock.NewRational(1, 25)})
	lo, hi, ok := tl.SubtitleBounds()
	if !ok || lo != clock.NewRational(1, 1) || hi != clock.NewRational(7, 1) {
		t.Fatalf("bounds = %s..%s ok=%v", lo, hi, ok)
	}
}

func TestParseConflictPolicy(t *testing.T) {
	for _, s := range []string{"", "last-wins", "keep-both", "reject"} {
		if _, err := ParseConflictPolicy(s); err != nil {
			t.Errorf("ParseConflictPolicy(%q): %v", s, err)
		}
	}
	if _, err := ParseConflictPolicy("nonsense"); err == nil {
		t.Fatal("expected error for unknown policy")
	}
}

func TestCrossTrackPriorityResolution(t *testing.T) {
	high := cueTrack("sub-high", 10, cue(0, 4, "high", 0))
	low := cueTrack("sub-low", 1, cue(2, 6, "low", 10))
	tl := Compose([]TrackInput{low, high}, Options{
		Policy: ConflictLastWins, FrameDuration: clock.NewRational(1, 25)})
	if len(tl.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v", tl.Conflicts)
	}
	c := tl.Conflicts[0]
	if c.Track != "sub-low" || c.Resolved != "trimmed-lower-priority" {
		t.Fatalf("conflict = %+v", c)
	}
	// The lower priority cue starts inside the winner, so it keeps only
	// its tail after the winner ends.
	for _, e := range tl.Entries {
		if e.Track == "sub-low" && (e.Start != clock.NewRational(4, 1) ||
			e.End != clock.NewRational(6, 1)) {
			t.Fatalf("low priority cue = %s..%s, want 4..6", e.Start, e.End)
		}
	}

	keep := Compose([]TrackInput{low, high}, Options{
		Policy: ConflictKeepBoth, FrameDuration: clock.NewRational(1, 25)})
	if keep.Conflicts[0].Resolved != "kept-both" {
		t.Fatalf("keep-both conflict = %+v", keep.Conflicts[0])
	}
	for _, e := range keep.Entries {
		if e.Track == "sub-low" && e.End != clock.NewRational(6, 1) {
			t.Fatalf("keep-both changed the low priority cue: %+v", e)
		}
	}

	reject := Compose([]TrackInput{low, high}, Options{
		Policy: ConflictReject, FrameDuration: clock.NewRational(1, 25)})
	if reject.SubtitleCount != 1 {
		t.Fatalf("reject kept %d subtitles", reject.SubtitleCount)
	}
}
