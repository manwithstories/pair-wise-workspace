package manifest

import (
	"strings"
	"testing"

	"mediad/internal/clock"
)

const sampleMPD = `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" profiles="urn:mpeg:dash:profile:isoff-live:2011"
     mediaPresentationDuration="PT12S" minBufferTime="PT2S">
  <Period id="p0" start="PT0S" duration="PT12S">
    <AdaptationSet mimeType="audio/mp4" codecs="mp4a.40.2">
      <SegmentTemplate timescale="48000" initialization="init-$RepresentationID$.m4s"
          media="seg-$RepresentationID$-$Number%05d$.m4s" startNumber="5"
          presentationTimeOffset="0">
        <SegmentTimeline>
          <S t="0" d="96000" r="2"/>
        </SegmentTimeline>
      </SegmentTemplate>
      <Representation id="a1" bandwidth="128000"/>
    </AdaptationSet>
    <AdaptationSet mimeType="video/mp4" codecs="avc1.4d401f" width="1280" height="720">
      <SegmentTemplate timescale="90000" duration="180000" startNumber="1"
          initialization="vinit.m4s" media="v-$Number%03d$.m4s"/>
      <Representation id="v1" bandwidth="2000000"/>
    </AdaptationSet>
  </Period>
</MPD>
`

func TestDASHSegmentTimeline(t *testing.T) {
	m, f, err := ParseDASH([]byte(sampleMPD), "manifest.mpd")
	if err != nil {
		t.Fatal(err)
	}
	if f.Len() != 0 {
		t.Fatalf("unexpected faults: %v", f.Err())
	}
	if len(m.Representations) != 2 {
		t.Fatalf("representations = %d", len(m.Representations))
	}
	audio := m.Representations[0]
	if audio.ID != "a1" || audio.InitURI != "init-a1.m4s" {
		t.Fatalf("audio rep = %+v", audio)
	}
	if len(audio.Segments) != 3 {
		t.Fatalf("audio segments = %d", len(audio.Segments))
	}
	if got := audio.Segments[0].URI; got != "seg-a1-00005.m4s" {
		t.Fatalf("uri = %s", got)
	}
	if got := audio.Segments[2].Sequence; got != 7 {
		t.Fatalf("sequence = %d", got)
	}
	// 96000 ticks at 48 kHz = 2 s = 180000 at 90 kHz canonical.
	if audio.Segments[0].Duration != 180000 || audio.Segments[1].Start != 180000 {
		t.Fatalf("canonical timeline wrong: %+v", audio.Segments)
	}
	if audio.Segments[0].NativeDuration != 96000 || audio.Segments[0].NativeTimescale != 48000 {
		t.Fatalf("native timeline wrong: %+v", audio.Segments[0])
	}
	video := m.Representations[1]
	if len(video.Segments) != 6 {
		t.Fatalf("video segments from duration = %d, want 6", len(video.Segments))
	}
	if video.Segments[0].URI != "v-001.m4s" || video.Segments[5].URI != "v-006.m4s" {
		t.Fatalf("video uris %s .. %s", video.Segments[0].URI, video.Segments[5].URI)
	}
}

func TestDASHRoundTrip(t *testing.T) {
	m, _, err := ParseDASH([]byte(sampleMPD), "m.mpd")
	if err != nil {
		t.Fatal(err)
	}
	out, err := SerializeDASH(m)
	if err != nil {
		t.Fatal(err)
	}
	m2, f, err := ParseDASH(out, "m2.mpd")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if f.Len() != 0 {
		t.Fatalf("round trip faults: %v\n%s", f.Err(), out)
	}
	matched, total := RoundTripScore(m, m2)
	if matched != total {
		t.Fatalf("round trip %d/%d\n%s", matched, total, out)
	}
	if !Equal(m, m2) {
		t.Fatalf("canonical mismatch\n%s", out)
	}
}

func TestDASHNegativeRepeatUsesPeriodDuration(t *testing.T) {
	mpd := `<MPD mediaPresentationDuration="PT10S"><Period duration="PT10S">
<AdaptationSet><SegmentTemplate timescale="1000" media="s$Number$.m4s">
<SegmentTimeline><S t="0" d="2000" r="-1"/></SegmentTimeline>
</SegmentTemplate><Representation id="r" bandwidth="1"/></AdaptationSet></Period></MPD>`
	m, f, err := ParseDASH([]byte(mpd), "p.mpd")
	if err != nil || f.Len() != 0 {
		t.Fatalf("%v %v", err, f.Err())
	}
	if got := len(m.Representations[0].Segments); got != 5 {
		t.Fatalf("segments = %d, want 5", got)
	}
}

func TestDASHMissingTimelineFault(t *testing.T) {
	mpd := `<MPD><Period><AdaptationSet>
<SegmentTemplate media="s$Number$.m4s" timescale="1000"/>
<Representation id="r" bandwidth="1"/></AdaptationSet></Period></MPD>`
	m, f, err := ParseDASH([]byte(mpd), "p.mpd")
	if err != nil {
		t.Fatal(err)
	}
	if m.SegmentCount() != 0 {
		t.Fatalf("fabricated segments: %+v", m.Segments())
	}
	if f.Len() == 0 {
		t.Fatal("expected faults for missing timeline")
	}
}

func TestExpandTemplateFormatting(t *testing.T) {
	rep := Representation{ID: "abc", Bandwidth: 42}
	if got := expandTemplate("$RepresentationID$-$Number%04d$-$Time$-$Bandwidth$.m4s", rep, 7, 9000); got != "abc-0007-9000-42.m4s" {
		t.Fatalf("got %q", got)
	}
	if got := expandTemplate("lit$$eral", rep, 1, 0); got != "lit$eral" {
		t.Fatalf("dollar escape = %q", got)
	}
	if got := expandTemplate("$Unknown$", rep, 1, 0); got != "$Unknown$" {
		t.Fatalf("unknown token = %q", got)
	}
}

func TestCanonicalConversion(t *testing.T) {
	m, _, err := ParseDASH([]byte(sampleMPD), "m.mpd")
	if err != nil {
		t.Fatal(err)
	}
	seg := m.Representations[0].Segments[0]
	if seg.Start != seg.NativeStart && seg.NativeStart != 0 {
		t.Fatalf("start mismatch")
	}
	tb := clock.Default()
	if got := tb.Seconds(seg.Duration); got != 2 {
		t.Fatalf("seconds = %v", got)
	}
	if strings.Contains(seg.URI, "$") {
		t.Fatalf("unexpanded uri %s", seg.URI)
	}
}
