package manifest

import (
	"strings"
	"testing"

	"mediacore/internal/clock"
)

var videoMeta = TrackMeta{ID: "v1", Kind: "video", Codec: "avc1", Timescale: 90000}

func TestParseHLSBasic(t *testing.T) {
	doc := `#EXTM3U
#EXT-X-TARGETDURATION:3
#EXTINF:2.000,
seg-1.m4s
#EXTINF:2.005,
seg-2.m4s
#EXTINF:1.5,
#EXT-X-GAP
seg-3.m4s
#EXT-X-ENDLIST
`
	tr, err := ParseHLS(strings.NewReader(doc), videoMeta)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Segments) != 3 {
		t.Fatalf("got %d segments", len(tr.Segments))
	}
	if tr.Segments[1].Dur != clock.Must(401, 200) {
		t.Fatalf("2.005 parsed as %v", tr.Segments[1].Dur)
	}
	if !tr.Segments[2].Gap {
		t.Fatal("third segment should be a gap")
	}
}

func TestParseHLSNegativeDuration(t *testing.T) {
	doc := "#EXTM3U\n#EXTINF:2.0,\na.m4s\n#EXTINF:-1.5,\nb.m4s\n"
	_, err := ParseHLS(strings.NewReader(doc), videoMeta)
	if clock.CodeOf(err) != clock.CodeNegativeDuration {
		t.Fatalf("want DURATION_NEGATIVE, got %v", err)
	}
	e := err.(*clock.Error)
	if e.Segment != 1 || e.Track != "v1" {
		t.Fatalf("error should point at track v1 segment 1: %v", e)
	}
}

func TestParseHLSRejectsPathEscape(t *testing.T) {
	for _, uri := range []string{"../etc/passwd", "/abs/x.m4s", "a/../../b.m4s", "http://evil/x.m4s"} {
		doc := "#EXTM3U\n#EXTINF:2.0,\n" + uri + "\n"
		if _, err := ParseHLS(strings.NewReader(doc), videoMeta); clock.CodeOf(err) != clock.CodePathEscape {
			t.Fatalf("uri %q: want PATH_ESCAPE, got %v", uri, err)
		}
	}
}

func TestParseHLSRejectsUnsupportedCodec(t *testing.T) {
	doc := "#EXTM3U\n#EXTINF:2.0,\na.m4s\n"
	meta := TrackMeta{ID: "v1", Kind: "video", Codec: "vp9", Timescale: 90000}
	if _, err := ParseHLS(strings.NewReader(doc), meta); clock.CodeOf(err) != clock.CodeCodecUnsupported {
		t.Fatalf("want CODEC_UNSUPPORTED, got %v", err)
	}
}

func TestParseHLSMissingHeader(t *testing.T) {
	doc := "#EXTINF:2.0,\na.m4s\n"
	if _, err := ParseHLS(strings.NewReader(doc), videoMeta); clock.CodeOf(err) != clock.CodeManifestInvalid {
		t.Fatalf("want MANIFEST_INVALID, got %v", err)
	}
}

func TestHLSRoundTrip(t *testing.T) {
	tr := Track{TrackMeta: videoMeta, Segments: []Segment{
		{Index: 0, URI: "a.m4s", Dur: clock.Must(2, 1)},
		{Index: 1, URI: "b.m4s", Dur: clock.Must(3, 2)},
	}}
	back, err := ParseHLS(strings.NewReader(FormatHLS(tr)), videoMeta)
	if err != nil {
		t.Fatal(err)
	}
	for i := range tr.Segments {
		if back.Segments[i].Dur.Cmp(tr.Segments[i].Dur) != 0 {
			t.Fatalf("segment %d duration %v != %v", i, back.Segments[i].Dur, tr.Segments[i].Dur)
		}
	}
}
