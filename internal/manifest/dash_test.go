package manifest

import (
	"strings"
	"testing"

	"mediacore/internal/clock"
)

const dashTimelineDoc = `<?xml version="1.0"?>
<MPD mediaPresentationDuration="PT8S">
  <Period>
    <AdaptationSet id="v1" contentType="video" codecs="avc1">
      <Representation id="v1">
        <SegmentTemplate timescale="90000" media="v_$Number%02d$.m4s" startNumber="0">
          <SegmentTimeline>
            <S t="1800" d="180000" r="1"/>
            <S t="378000" d="180000" r="1"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

func TestParseDASHTimeline(t *testing.T) {
	pl, err := ParseDASH(strings.NewReader(dashTimelineDoc))
	if err != nil {
		t.Fatal(err)
	}
	if pl.Format != "dash" || len(pl.Tracks) != 1 {
		t.Fatalf("bad playlist: %+v", pl)
	}
	segs := pl.Tracks[0].Segments
	// 2 segments + 1 hole + 2 segments
	if len(segs) != 5 {
		t.Fatalf("got %d segments, want 5: %+v", len(segs), segs)
	}
	if segs[0].URI != "v_01.m4s" || segs[0].Dur != clock.Must(2, 1) {
		t.Fatalf("bad first segment: %+v", segs[0])
	}
	// hole = 378000/90000 - (1800/90000 + 2*2s) = 4.2 - 4.02 = 0.18s
	if !segs[2].Gap || segs[2].Dur != clock.Must(9, 50) {
		t.Fatalf("hole should be 0.18s gap: %+v", segs[2])
	}
	if segs[3].URI != "v_03.m4s" {
		t.Fatalf("bad URI after hole: %q", segs[3].URI)
	}
}

func TestParseDASHFixedDuration(t *testing.T) {
	doc := `<MPD mediaPresentationDuration="PT6S"><Period>
	  <AdaptationSet id="a1" contentType="audio" codecs="mp4a">
	    <Representation id="a1">
	      <SegmentTemplate timescale="48000" duration="96000" media="a_$Number$.m4s"/>
	    </Representation>
	  </AdaptationSet></Period></MPD>`
	pl, err := ParseDASH(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Tracks[0].Segments) != 3 {
		t.Fatalf("got %d segments, want 3", len(pl.Tracks[0].Segments))
	}
}

func TestParseDASHNegativeDuration(t *testing.T) {
	doc := `<MPD><Period><AdaptationSet id="v1" contentType="video" codecs="avc1">
	  <Representation id="v1"><SegmentTemplate timescale="90000" media="v_$Number$.m4s">
	    <SegmentTimeline><S t="0" d="-5" r="0"/></SegmentTimeline>
	  </SegmentTemplate></Representation></AdaptationSet></Period></MPD>`
	_, err := ParseDASH(strings.NewReader(doc))
	if clock.CodeOf(err) != clock.CodeNegativeDuration {
		t.Fatalf("want DURATION_NEGATIVE, got %v", err)
	}
}

func TestParseDASHBadTimescale(t *testing.T) {
	doc := `<MPD><Period><AdaptationSet id="v1" contentType="video" codecs="avc1">
	  <Representation id="v1"><SegmentTemplate timescale="0" duration="10" media="v_$Number$.m4s"/>
	  </Representation></AdaptationSet></Period></MPD>`
	_, err := ParseDASH(strings.NewReader(doc))
	if clock.CodeOf(err) != clock.CodeTimescaleInvalid {
		t.Fatalf("want TIMESCALE_INVALID, got %v", err)
	}
}

func TestParseDASHTickOverflow(t *testing.T) {
	doc := `<MPD><Period><AdaptationSet id="v1" contentType="video" codecs="avc1">
	  <Representation id="v1"><SegmentTemplate timescale="90000" media="v_$Number$.m4s">
	    <SegmentTimeline><S t="9223372036854775807" d="180000" r="3"/></SegmentTimeline>
	  </SegmentTemplate></Representation></AdaptationSet></Period></MPD>`
	_, err := ParseDASH(strings.NewReader(doc))
	if clock.CodeOf(err) != clock.CodeTickOverflow {
		t.Fatalf("want TICK_OVERFLOW, got %v", err)
	}
}

func TestParseISO8601Duration(t *testing.T) {
	d, err := ParseISO8601Duration("PT1H2M3.5S")
	if err != nil {
		t.Fatal(err)
	}
	if d != clock.Must(7447, 2) {
		t.Fatalf("got %v, want 3723.5s", d)
	}
	if _, err := ParseISO8601Duration("P1D"); err == nil {
		t.Fatal("P1D should be rejected")
	}
}
