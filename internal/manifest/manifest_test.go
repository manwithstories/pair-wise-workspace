package manifest

import (
	"strings"
	"testing"

	"mediaroute/internal/model"
)

const goodHLS = `#EXTM3U
#EXT-X-MEDIA-SEQUENCE:7
#EXTINF:2.0,
seg0.ts
#EXTINF:2.0,
seg1.ts
`

func TestHLSGood(t *testing.T) {
	segs, err := ParseHLS(strings.NewReader(goodHLS), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 || segs[0].Duration != 180000 || segs[1].Start != 180000 {
		t.Fatalf("bad segments: %+v", segs)
	}
}

func TestHLSMissingDuration(t *testing.T) {
	_, err := ParseHLS(strings.NewReader("#EXTM3U\nseg0.ts\n"), "v1")
	me, ok := err.(*model.MediaError)
	if !ok || me.Code != model.ErrManifest {
		t.Fatalf("want ErrManifest, got %v", err)
	}
	if me.Location != "line 2" {
		t.Fatalf("want line 2, got %s", me.Location)
	}
}

func TestHLSDuplicateConflict(t *testing.T) {
	pl := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:5\n#EXTINF:2,\na.ts\n" +
		"#EXT-X-MEDIA-SEQUENCE:4\n#EXTINF:2,\nb.ts\n"
	_, err := ParseHLS(strings.NewReader(pl), "v1")
	me, ok := err.(*model.MediaError)
	if !ok || me.Code != model.ErrDuplicate || me.Seq != 5 {
		t.Fatalf("want duplicate conflict at seq 5, got %v", err)
	}
}

const goodMPD = `<MPD><Period><AdaptationSet>
<SegmentList timescale="90000" duration="180000">
<SegmentURL media="s0.m4s"/><SegmentURL media="s1.m4s"/>
</SegmentList></AdaptationSet></Period></MPD>`

func TestDASHGood(t *testing.T) {
	segs, err := ParseDASH(strings.NewReader(goodMPD), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 || segs[1].Start != 180000 {
		t.Fatalf("bad segments: %+v", segs)
	}
	deps := Dependencies(segs)
	if len(deps) != 1 || deps[0] != [2]int{1, 0} {
		t.Fatalf("bad deps: %v", deps)
	}
}

func TestDASHMissingDuration(t *testing.T) {
	bad := `<MPD><Period><AdaptationSet>
<SegmentList><SegmentURL media="s0.m4s"/></SegmentList>
</AdaptationSet></Period></MPD>`
	_, err := ParseDASH(strings.NewReader(bad), "v1")
	me, ok := err.(*model.MediaError)
	if !ok || me.Code != model.ErrManifest ||
		!strings.Contains(me.Location, "@duration") {
		t.Fatalf("want duration error with XML path, got %v", err)
	}
}

func TestDASHDuplicateURI(t *testing.T) {
	bad := `<MPD><Period><AdaptationSet>
<SegmentList duration="90000">
<SegmentURL media="s0.m4s"/><SegmentURL media="s0.m4s"/>
</SegmentList></AdaptationSet></Period></MPD>`
	_, err := ParseDASH(strings.NewReader(bad), "v1")
	me, ok := err.(*model.MediaError)
	if !ok || me.Code != model.ErrDuplicate {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
}
