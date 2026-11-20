package manifest

import (
	"strings"
	"testing"

	"mediamap/internal/fault"
)

const mediaPlaylist = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:4.000,
seg_0.m4s
seg_1.m4s
#EXTINF:4.000,
seg_2.m4s
#EXT-X-ENDLIST
`

func TestHLSMediaDerivedBoundary(t *testing.T) {
	rep, faults := ParseHLSMedia("t", "r0", []byte(mediaPlaylist))
	if len(rep.Segments) != 3 {
		t.Fatalf("segments: got %d", len(rep.Segments))
	}
	if rep.Segments[1].Derived != true || rep.Segments[1].Duration != 4*90000 {
		t.Fatalf("seg_1 should be derived from target: %+v", rep.Segments[1])
	}
	if rep.Segments[2].Start != 8*90000 {
		t.Fatalf("cumulative start: got %d", rep.Segments[2].Start)
	}
	if faults.HasFatal() {
		t.Fatalf("unexpected fatal: %v", faults)
	}
	// The missing EXTINF must be reported with the byte offset of the URI line.
	var found bool
	for _, f := range faults {
		if f.Offset >= 0 && strings.Contains(f.Message, "seg_1.m4s") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no located fault for missing EXTINF: %v", faults)
	}
}

func TestHLSMediaStartOffset(t *testing.T) {
	pl := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-START:TIME-OFFSET=0.012\n" +
		"#EXTINF:4.000,\ns0.m4s\n"
	rep, _ := ParseHLSMedia("t", "r", []byte(pl))
	if rep.StartOffset != 1080 { // 12 ms in 90 kHz ticks
		t.Fatalf("start offset: got %d want 1080", rep.StartOffset)
	}
}

func TestHLSMediaEmptyIsFatal(t *testing.T) {
	_, faults := ParseHLSMedia("t", "r", []byte("#EXTM3U\n"))
	if !faults.HasFatal() {
		t.Fatal("empty playlist must be fatal")
	}
}

func TestHLSMaster(t *testing.T) {
	m := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\nlo.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=2400000\nhi.m3u8\n"
	variants, faults := ParseHLSMaster("t", []byte(m))
	if len(variants) != 2 || variants[1].Bandwidth != 2400000 || variants[0].URI != "lo.m3u8" {
		t.Fatalf("variants: %+v", variants)
	}
	if faults.HasFatal() {
		t.Fatalf("unexpected fatal: %v", faults)
	}
}

const mpd = `<MPD mediaPresentationDuration="PT12S">
  <Period>
    <AdaptationSet contentType="video">
      <Representation id="v1" bandwidth="800000">
        <SegmentTemplate timescale="90000" startNumber="0" media="v1_$Number$.m4s">
          <SegmentTimeline>
            <S t="0" d="360000" r="2"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
    <AdaptationSet contentType="audio">
      <Representation id="a1" bandwidth="64000">
        <SegmentTemplate timescale="48000" duration="192000" startNumber="1" media="a1_$Number$.m4s"/>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

func TestDASHTimelineAndDerived(t *testing.T) {
	tracks, faults := ParseDASH([]byte(mpd))
	if len(tracks) != 2 {
		t.Fatalf("tracks: got %d", len(tracks))
	}
	v := tracks[0].Representations[0]
	if len(v.Segments) != 3 || v.Segments[1].Start != 4*90000 {
		t.Fatalf("timeline expansion: %+v", v.Segments)
	}
	if v.Segments[0].URI != "v1_0.m4s" {
		t.Fatalf("template URI: %q", v.Segments[0].URI)
	}
	a := tracks[1]
	if a.Confidence != Derived {
		t.Fatalf("audio should be derived confidence: %s", a.Confidence)
	}
	segs := a.Representations[0].Segments
	if len(segs) != 3 || segs[0].URI != "a1_1.m4s" || !segs[0].Derived {
		t.Fatalf("derived segments: %+v", segs)
	}
	if faults.HasFatal() {
		t.Fatalf("unexpected fatal: %v", faults)
	}
}

func TestDASHMalformedXML(t *testing.T) {
	_, faults := ParseDASH([]byte("<MPD><Period>"))
	if !faults.HasFatal() {
		t.Fatal("malformed MPD must be fatal")
	}
}

func TestParseDecimalExact(t *testing.T) {
	r, err := parseDecimal("4.000")
	if err != nil || r.Num != 4 || r.Den != 1 {
		t.Fatalf("4.000: %+v err=%v", r, err)
	}
	r, err = parseDecimal("-0.012")
	if err != nil || r.Num != -3 || r.Den != 250 {
		t.Fatalf("-0.012: %+v err=%v", r, err)
	}
	if _, err := parseDecimal("1.2.3"); err == nil {
		t.Fatal("malformed decimal must fail")
	}
}

var _ = fault.NoOffset // keep fault import if assertions change
