package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"avrepair/internal/media"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseHLSMediaWithDiscontinuity(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "video_000.ts", "x")
	write(t, dir, "video_001.ts", "x")
	write(t, dir, "video_002.ts", "x")
	pl := write(t, dir, "video.m3u8", `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:5
#EXTINF:4.000,
video_000.ts
#EXTINF:4.000,
video_001.ts
#EXT-X-DISCONTINUITY
#EXTINF:4.000,
video_002.ts
#EXT-X-ENDLIST
`)
	m, err := Load(pl)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != "hls" || len(m.Entries) != 3 {
		t.Fatalf("manifest: %+v", m)
	}
	if !m.Entries[2].Discontinuity || m.Entries[2].DeclaredStart.MillisRounded() != 8000 {
		t.Fatalf("entry 2: %+v", m.Entries[2])
	}
	if m.Entries[0].Kind != media.TrackVideo {
		t.Fatalf("kind inference: %+v", m.Entries[0])
	}
}

func TestParseHLSMasterAndVariantRejection(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "v0.ts", "x")
	write(t, dir, "a0.ts", "x")
	write(t, dir, "video.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nv0.ts\n#EXT-X-ENDLIST\n")
	write(t, dir, "audio.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\na0.ts\n#EXT-X-ENDLIST\n")
	idx := write(t, dir, "index.m3u8", `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=800000,CODECS="avc1.4d401e",AUDIO="aud"
video.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=64000,CODECS="mp4a.40.2",AUDIO="aud"
audio.m3u8
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="main",DEFAULT=YES,URI="audio.m3u8"
`)
	m, err := Load(idx)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("entries: %d", len(m.Entries))
	}
	write(t, dir, "remote.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nhttps://example.com/x.ts\n#EXT-X-ENDLIST\n")
	if _, err := Load(filepath.Join(dir, "remote.m3u8")); err == nil {
		t.Fatal("remote segment URI accepted")
	}
}

func TestParseHLSTraversalAndLimit(t *testing.T) {
	dir := t.TempDir()
	pl := write(t, dir, "bad.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\n../../etc/passwd\n#EXT-X-ENDLIST\n")
	if _, err := Load(pl); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestParseDASHRepresentations(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "v0001.m4s", "x")
	write(t, dir, "a0001.m4s", "x")
	mpd := write(t, dir, "manifest.mpd", `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static">
  <Period id="p0">
    <AdaptationSet id="1" contentType="video" mimeType="video/mp4">
      <SegmentTemplate timescale="90000" media="v$Number%04d$.m4s" startNumber="1"/>
      <Representation id="v1" codecs="avc1.4d401e" bandwidth="800000">
        <SegmentTemplate timescale="90000" media="v$Number%04d$.m4s">
          <SegmentTimeline>
            <S t="0" d="180000" r="1"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
    <AdaptationSet id="2" contentType="audio" mimeType="audio/mp4">
      <Representation id="a1" codecs="mp4a.40.2" bandwidth="64000">
        <SegmentTemplate timescale="48000" media="a$Number%04d$.m4s">
          <SegmentTimeline><S t="0" d="96000"/></SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`)
	m, err := Load(mpd)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != "dash" || len(m.Entries) != 3 {
		t.Fatalf("manifest: %+v", m)
	}
	v := m.Entries[0]
	if v.Kind != media.TrackVideo || v.Duration.MillisRounded() != 2000 {
		t.Fatalf("video entry: %+v", v)
	}
	if v.DeclaredStart.MillisRounded() != 0 {
		t.Fatalf("video start: %+v", v)
	}
	a := m.Entries[2]
	if a.Kind != media.TrackAudio || a.Duration.MillisRounded() != 2000 {
		t.Fatalf("audio entry: %+v", a)
	}
}

func TestParseDASHInvalidTimescale(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "v0001.m4s", "x")
	mpd := write(t, dir, "bad.mpd", `<MPD><Period><AdaptationSet contentType="video">
<Representation id="v1"><SegmentTemplate timescale="-5" media="v$Number%04d$.m4s" duration="2"/></Representation>
</AdaptationSet></Period></MPD>`)
	if _, err := Load(mpd); err == nil {
		t.Fatal("negative timescale accepted")
	}
}

func TestParseDASHTemplateTokenValidation(t *testing.T) {
	if _, err := dashExpand("seg-$Number$.m4s", 3, 0, "v1", 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := expandToken("RepresentationID", 0, 0, "../x", 0); ok {
		t.Fatal("traversal representation id accepted")
	}
	if _, ok := expandToken("Bandwidth%05d", 0, 0, "v1", 800000); !ok {
		t.Fatal("bandwidth token should expand")
	}
	if _, err := dashExpand("seg-$Unknown$.m4s", 1, 0, "v1", 0); err == nil {
		t.Fatal("unknown token accepted")
	}
}
