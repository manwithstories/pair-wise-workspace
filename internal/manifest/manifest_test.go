package manifest

import (
	"strings"
	"testing"
)

const sampleHLS = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:907
#EXT-X-MAP:URI="init.mp4"
#EXTINF:2.000,
seg_000.m4s
#EXTINF:2.000,
seg_001.m4s
#EXT-X-DISCONTINUITY
#EXTINF:2.000,
seg_002.m4s
#EXTINF:1.500,
seg_003.m4s
`

func TestParseHLSDiscontinuityMapSequence(t *testing.T) {
	m, err := ParseHLS(strings.NewReader(sampleHLS))
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != KindHLS || m.MediaSequence != 907 {
		t.Fatalf("kind/seq = %s/%d", m.Kind, m.MediaSequence)
	}
	if len(m.Segments) != 4 {
		t.Fatalf("segments=%d want 4", len(m.Segments))
	}
	if m.Segments[0].Sequence != 907 || m.Segments[3].Sequence != 910 {
		t.Fatalf("sequence numbering wrong: %d..%d", m.Segments[0].Sequence, m.Segments[3].Sequence)
	}
	if m.Segments[0].MapURI != "init.mp4" {
		t.Fatalf("map uri=%q", m.Segments[0].MapURI)
	}
	if !m.Segments[2].Discontinuity || m.Segments[0].Discontinuity || m.Segments[3].Discontinuity {
		t.Fatal("discontinuity must attach only to segment 2")
	}
	if m.Segments[3].DurationSec != 1.5 {
		t.Fatalf("duration=%v", m.Segments[3].DurationSec)
	}
	var disc, maps int
	for _, a := range m.Anchors {
		switch a.Kind {
		case AnchorDiscontinuity:
			disc++
			if a.SegmentIndex != 2 {
				t.Fatalf("disc anchor at %d", a.SegmentIndex)
			}
		case AnchorMap:
			maps++
		}
	}
	if disc != 1 || maps != 1 {
		t.Fatalf("anchors disc=%d map=%d", disc, maps)
	}
}

func TestParseHLSRejectsRemoteAndTraversal(t *testing.T) {
	cases := map[string]string{
		"remote":   "#EXTM3U\n#EXTINF:1,\nhttps://evil/x.m4s\n",
		"traversal": "#EXTM3U\n#EXT-X-MAP:URI=\"../../init.mp4\"\n#EXTINF:1,\na.m4s\n",
	}
	for name, doc := range cases {
		if _, err := ParseHLS(strings.NewReader(doc)); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
}

const sampleMPD = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static">
  <Period>
    <AdaptationSet contentType="video">
      <Representation id="v1" codecs="avc1.64001f">
        <SegmentTemplate timescale="90000" startNumber="100"
          presentationTimeOffset="180000"
          media="video/seg_$Number%03d$.m4s" initialization="video/init.mp4">
          <SegmentTimeline>
            <S t="900000" d="180000"/>
            <S d="90000" r="1"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
    <AdaptationSet contentType="audio">
      <Representation id="a1" codecs="mp4a.40.2" audioSamplingRate="48000">
        <AudioChannelConfiguration value="2"/>
        <SegmentTemplate timescale="48000" startNumber="100"
          media="audio/seg_$Number%03d$.m4s">
          <SegmentTimeline>
            <S t="480000" d="96000" r="2"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

func TestParseDASHTemplateTimelinePTO(t *testing.T) {
	m, err := ParseDASH(strings.NewReader(sampleMPD))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tracks) != 2 {
		t.Fatalf("tracks=%d", len(m.Tracks))
	}
	v, _ := m.Track("v1")
	if v.Kind != TrackVideo || v.Timescale != 90000 {
		t.Fatalf("video track wrong: %+v", v)
	}
	a, _ := m.Track("a1")
	if a.SampleRate != 48000 || a.Channels != 2 || a.Timescale != 48000 {
		t.Fatalf("audio track wrong: %+v", a)
	}
	if m.PresentationTimeOffset != 180000 {
		t.Fatalf("pto=%d", m.PresentationTimeOffset)
	}
	// Video: first S + r=1 second S => 3 video segments total across
	// both reps is 6; check the video subset explicitly.
	var vids []Segment
	for _, s := range m.Segments {
		if s.TrackID == "v1" {
			vids = append(vids, s)
		}
	}
	if len(vids) != 3 {
		t.Fatalf("video segments=%d want 3", len(vids))
	}
	if vids[0].StartTicks != 900000 || vids[0].DurationTicks != 180000 {
		t.Fatalf("first video seg %+v", vids[0])
	}
	if vids[1].StartTicks != 1080000 || vids[2].StartTicks != 1170000 {
		t.Fatalf("timeline cursor expansion wrong: %+v %+v", vids[1], vids[2])
	}
	if vids[2].URI != "video/seg_102.m4s" {
		t.Fatalf("template expansion=%q", vids[2].URI)
	}
	if vids[0].MapURI != "video/init.mp4" {
		t.Fatalf("init=%q", vids[0].MapURI)
	}
}

func TestParseDASHRejectsRemoteMedia(t *testing.T) {
	doc := strings.Replace(sampleMPD, `media="video/seg_$Number%03d$.m4s"`,
		`media="https://cdn.example/seg_$Number$.m4s"`, 1)
	if _, err := ParseDASH(strings.NewReader(doc)); err == nil {
		t.Fatal("remote DASH media must be rejected")
	}
}

func TestMetadataUniqueInference(t *testing.T) {
	got := ReconcileMetadata(TrackDesc{ID: "a", Channels: 2, SampleRate: 48000})
	if got.ChannelLayout != "stereo" || got.HasMetadataConflict() {
		t.Fatalf("2 channels must infer stereo uniquely: %+v", got)
	}
	mono := ReconcileMetadata(TrackDesc{Channels: 1})
	if mono.ChannelLayout != "mono" {
		t.Fatalf("1 channel must infer mono, got %q", mono.ChannelLayout)
	}
}

func TestMetadataAmbiguousAndConflicting(t *testing.T) {
	// Six channels do not identify a unique layout: mark conflict and
	// do not fabricate one.
	amb := ReconcileMetadata(TrackDesc{ID: "x", Channels: 6})
	if amb.ChannelLayout != "" || !amb.HasMetadataConflict() {
		t.Fatalf("6ch must stay layout-less with conflict, got %+v", amb)
	}
	// Explicit contradiction between channel count and layout.
	bad := ReconcileMetadata(TrackDesc{Channels: 6, ChannelLayout: "stereo"})
	if !bad.HasMetadataConflict() {
		t.Fatal("6ch + stereo must conflict")
	}
	// Disagreement between metadata sources must be recorded while the
	// earlier container value is preserved.
	src := ReconcileMetadata(
		TrackDesc{ID: "a", SampleRate: 48000, ColorInfo: "bt709"},
		MetadataHint{Source: "sidecar", SampleRate: 44100},
	)
	if !src.HasMetadataConflict() || src.SampleRate != 48000 {
		t.Fatalf("source conflict not preserved: %+v", src)
	}
}
