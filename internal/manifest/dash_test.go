package manifest

import (
	"testing"
	"testing/fstest"

	"mediad/internal/model"
)

func TestParseDASH_BasicTimeline(t *testing.T) {
	mpd := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet id="studio">
<SegmentTemplate timescale="1000" media="s-$Number$.m4s" startNumber="1">
<SegmentTimeline><S t="0" d="6000"/><S d="6000"/></SegmentTimeline>
</SegmentTemplate></AdaptationSet></Period></MPD>`
	fsys := fstest.MapFS{
		"manifest.mpd": {Data: []byte(mpd)},
		"s-1.m4s":      {Data: make([]byte, 10)},
		"s-2.m4s":      {Data: make([]byte, 20)},
	}
	tl, err := ParseDASH(fsys, "manifest.mpd")
	if err != nil {
		t.Fatalf("ParseDASH: %v", err)
	}
	segs := tl.Tracks[model.TrackStudio]
	if len(segs) != 2 {
		t.Fatalf("segments = %d, want 2", len(segs))
	}
	if segs[0].StartMS != 0 || segs[0].DurationMS != 6000 || segs[0].Timescale != 1000 {
		t.Fatalf("seg0 = %+v", segs[0])
	}
	if segs[1].StartMS != 6000 || segs[1].URI != "s-2.m4s" {
		t.Fatalf("seg1 = %+v", segs[1])
	}
	if tl.Partial {
		t.Fatalf("complete MPD must not be partial: %+v", tl.Missing)
	}
}

func TestParseDASH_RepeatsAndTimeTemplate(t *testing.T) {
	mpd := `<MPD><Period><AdaptationSet id="field">
<SegmentTemplate timescale="90000" media="f-$Time$.m4s">
<SegmentTimeline><S t="90000" d="90000" r="2"/></SegmentTimeline>
</SegmentTemplate></AdaptationSet></Period></MPD>`
	fsys := fstest.MapFS{
		"manifest.mpd": {Data: []byte(mpd)},
		"f-90000.m4s":  {Data: []byte("a")},
		"f-180000.m4s": {Data: []byte("b")},
		"f-270000.m4s": {Data: []byte("c")},
	}
	tl, err := ParseDASH(fsys, "manifest.mpd")
	if err != nil {
		t.Fatalf("ParseDASH: %v", err)
	}
	segs := tl.Tracks[model.TrackField]
	if len(segs) != 3 {
		t.Fatalf("r=2 must expand to 3 entries, got %d", len(segs))
	}
	for i, want := range []int64{1000, 2000, 3000} {
		if segs[i].StartMS != want {
			t.Fatalf("seg%d start = %d, want %d", i, segs[i].StartMS, want)
		}
	}
	if segs[2].URI != "f-270000.m4s" {
		t.Fatalf("last URI = %q", segs[2].URI)
	}
}

func TestParseDASH_MissingFragmentIsPartial(t *testing.T) {
	mpd := `<MPD><Period><AdaptationSet id="voiceover">
<SegmentTemplate timescale="1000" media="v-$Number$.m4s">
<SegmentTimeline><S t="0" d="5000"/><S d="5000"/></SegmentTimeline>
</SegmentTemplate></AdaptationSet></Period></MPD>`
	fsys := fstest.MapFS{
		"manifest.mpd": {Data: []byte(mpd)},
		"v-1.m4s":      {Data: []byte("ok")},
	}
	tl, err := ParseDASH(fsys, "manifest.mpd")
	if err != nil {
		t.Fatalf("partial index must not hard-fail: %v", err)
	}
	if !tl.Partial || len(tl.Missing) != 1 || tl.Missing[0].Index != 1 {
		t.Fatalf("missing marker wrong: %+v", tl.Missing)
	}
	if len(tl.Tracks[model.TrackVoice]) != 1 {
		t.Fatalf("confirmed segment must remain")
	}
}

func TestParseDASH_InvalidInputs(t *testing.T) {
	cases := map[string]string{
		"no period":    `<MPD></MPD>`,
		"negative d":   `<MPD><Period><AdaptationSet id="studio"><SegmentTemplate timescale="1000" media="x-$Number$.m4s"><SegmentTimeline><S t="0" d="-1"/></SegmentTimeline></SegmentTemplate></AdaptationSet></Period></MPD>`,
		"unknown role": `<MPD><Period><AdaptationSet id="mystery"><SegmentTemplate media="x.m4s"><SegmentTimeline><S d="1"/></SegmentTimeline></SegmentTemplate></AdaptationSet></Period></MPD>`,
		"no timeline":  `<MPD><Period><AdaptationSet id="studio"><SegmentTemplate media="x.m4s"/></AdaptationSet></Period></MPD>`,
		"broken xml":   `<MPD><Period>`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			fsys := fstest.MapFS{"manifest.mpd": {Data: []byte(doc)}}
			_, err := ParseDASH(fsys, "manifest.mpd")
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestParseDASH_DefaultTimescaleAndStartNumber(t *testing.T) {
	mpd := `<MPD><Period><AdaptationSet id="studio">
<SegmentTemplate media="x-$Number$.m4s">
<SegmentTimeline><S t="0" d="2"/></SegmentTimeline>
</SegmentTemplate></AdaptationSet></Period></MPD>`
	fsys := fstest.MapFS{
		"manifest.mpd": {Data: []byte(mpd)},
		"x-1.m4s":      {Data: []byte("x")},
	}
	tl, err := ParseDASH(fsys, "manifest.mpd")
	if err != nil {
		t.Fatalf("ParseDASH: %v", err)
	}
	seg := tl.Tracks[model.TrackStudio][0]
	if seg.Timescale != 1 || seg.DurationMS != 2000 {
		t.Fatalf("default timescale wrong: %+v", seg)
	}
}
