package manifest

import (
	"encoding/xml"
	"strings"
	"testing"

	"aircheck/internal/media"
)

const mpdText = `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" profiles="urn:mpeg:dash:profile:isoff-live:2011"
     mediaPresentationDuration="PT10S" minBufferTime="PT1S">
  <BaseURL>dash/</BaseURL>
  <Period id="p0" start="PT0S" duration="PT10S">
    <AdaptationSet id="v" mimeType="video/mp4" codecs="avc1.64001f" frameRate="30">
      <BaseURL>video/</BaseURL>
      <SegmentTemplate initialization="$RepresentationID$/init.mp4"
                       media="$RepresentationID$/$Number$.m4s"
                       startNumber="1" timescale="90000" duration="90000"/>
      <Representation id="720p" bandwidth="2000000" width="1280" height="720" codecs="avc1.64001f"/>
      <Representation id="360p" bandwidth="800000" width="640" height="360" codecs="avc1.4d401e"/>
    </AdaptationSet>
    <AdaptationSet id="a" mimeType="audio/mp4" codecs="mp4a.40.2" lang="en">
      <BaseURL>audio/</BaseURL>
      <SegmentTemplate initialization="$RepresentationID$/init.mp4"
                       media="$RepresentationID$/$Number$.m4s"
                       startNumber="1" timescale="48000" duration="48000"/>
      <Representation id="aac" bandwidth="128000" audioSamplingRate="48000" codecs="mp4a.40.2"/>
    </AdaptationSet>
  </Period>
</MPD>
`

func TestParseDashMPD(t *testing.T) {
	clk := media.MustClock(720000)
	d, err := ParseDashMPD(Artifact{Name: "manifest.mpd", Data: []byte(mpdText)}, HSLOptions{Root: "media", Clock: clk, MaxDurationSec: 60})
	if err != nil {
		t.Fatalf("ParseDashMPD: %v", err)
	}
	if len(d.Tracks) != 3 {
		t.Fatalf("tracks %d", len(d.Tracks))
	}
	v, a := d.SelectTracks(2000000)
	if v == nil || v.RepID != "720p" || v.Bandwidth != 2000000 {
		t.Fatalf("video selection %+v", v)
	}
	if a == nil || a.RepID != "aac" || a.SampleRate != 48000 {
		t.Fatalf("audio selection %+v", a)
	}
	if len(v.Segments) != 10 || len(a.Segments) != 10 {
		t.Fatalf("segment counts v=%d a=%d", len(v.Segments), len(a.Segments))
	}
	if v.Duration.Ticks() != 7200000 || a.Duration.Ticks() != 7200000 {
		t.Fatalf("durations v=%d a=%d", v.Duration.Ticks(), a.Duration.Ticks())
	}
	s := v.Segments[1]
	if s.Ref.URI != "dash/video/720p/2.m4s" || s.TotalTicks != 90000 {
		t.Fatalf("segment %+v", s)
	}
	if v.Init == nil || v.Init.URI != "dash/video/720p/init.mp4" {
		t.Fatalf("init %+v", v.Init)
	}
	if s.Init == nil || s.Init.URI != v.Init.URI {
		t.Fatalf("segment init not inherited: %+v", s.Init)
	}
}

func TestParseDashTimelineAndPTO(t *testing.T) {
	old := `startNumber="1" timescale="90000" duration="90000"/>`
	newT := `startNumber="7" timescale="90000" presentationTimeOffset="1800">
          <SegmentTimeline><S t="1800" d="90000" r="9"/></SegmentTimeline>
        </SegmentTemplate>`
	text := strings.Replace(mpdText, old, newT, 1)
	d, err := ParseDashMPD(Artifact{Name: "m.mpd", Data: []byte(text)}, HSLOptions{Root: "media", Clock: media.MustClock(720000)})
	if err != nil {
		t.Fatalf("ParseDashMPD: %v", err)
	}
	v, _ := d.SelectTracks(2000000)
	if len(v.Segments) != 10 {
		t.Fatalf("timeline segments %d", len(v.Segments))
	}
	if v.Segments[0].Seq != 7 || v.Segments[0].TotalTicks != 0 || v.Segments[0].Ref.URI != "dash/video/720p/7.m4s" {
		t.Fatalf("first segment %+v", v.Segments[0])
	}
	if v.Segments[9].TotalTicks != 9*90000 {
		t.Fatalf("last start %d", v.Segments[9].TotalTicks)
	}
}

func TestParseDashRejects(t *testing.T) {
	cases := []struct {
		name string
		text string
		kind media.FaultKind
	}{
		{"no period", `<MPD></MPD>`, media.FaultManifestSyntax},
		{"escape", strings.Replace(mpdText, "<BaseURL>dash/</BaseURL>", "<BaseURL>../</BaseURL>", 1), media.FaultManifestRange},
		{"absolute media", strings.Replace(mpdText, `media="$RepresentationID$/$Number$.m4s"`,
			`media="http://evil/$Number$.m4s"`, 1), media.FaultManifestRange},
		{"no template", strings.Replace(mpdText, "<SegmentTemplate initialization=\"$RepresentationID$/init.mp4\"\n                       media=\"$RepresentationID$/$Number$.m4s\"\n                       startNumber=\"1\" timescale=\"90000\" duration=\"90000\"/>", "", 1), media.FaultUnsupportedCodec},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseDashMPD(Artifact{Name: "m.mpd", Data: []byte(c.text)}, HSLOptions{Root: "media", Clock: media.MustClock(720000)})
			if err == nil {
				t.Fatal("expected rejection")
			}
			if media.KindOf(err) != c.kind {
				t.Fatalf("kind %v want %v: %v", media.KindOf(err), c.kind, err)
			}
		})
	}
}

func TestParseISODuration(t *testing.T) {
	cases := map[string]int64{"PT10S": 10, "PT1M30S": 90, "PT1H": 3600, "P1DT2H": 93600, "PT0S": 0}
	for s, sec := range cases {
		d, err := ParseISODuration(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if d.Ticks() != sec*1000 {
			t.Fatalf("%q -> %d ms", s, d.Ticks())
		}
	}
	for _, bad := range []string{"", "10S", "PT", "PT10X", "P1Y", "PT1.5S"} {
		if _, err := ParseISODuration(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if got := FormatISODuration(media.MustClock(1000).FromSeconds(10)); got != "PT10S" {
		t.Fatalf("format %q", got)
	}
}

func TestXMLDecoderFieldNames(t *testing.T) {
	// Guards the struct tags used by ParseDashMPD.
	var m MPD
	if err := xml.Unmarshal([]byte(mpdText), &m); err != nil {
		t.Fatalf("xml: %v", err)
	}
	if m.XMLName.Local != "MPD" || len(m.Periods) != 1 {
		t.Fatalf("decoded %+v", m.XMLName)
	}
}
