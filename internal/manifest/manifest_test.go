package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediascan/internal/model"
)

func testResolver(t *testing.T) *Resolver {
	t.Helper()
	r, err := NewResolver(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResolverRejectsEscape(t *testing.T) {
	r := testResolver(t)
	bad := []string{
		"../outside.m4s",
		"a/../../b.m4s",
		"/etc/passwd",
		"https://cdn.example/x.m4s",
		"a\x00b.m4s",
		"C:\\media\\x.m4s",
		"",
	}
	for _, uri := range bad {
		if _, err := r.Resolve(uri); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("Resolve(%q) = %v, want ErrUnsafePath", uri, err)
		}
	}
	good, err := r.Resolve("media/seg-1.m4s")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(good, r.Root) {
		t.Fatalf("resolved %q outside root %q", good, r.Root)
	}
}

func TestParseHLSMediaPlaylist(t *testing.T) {
	r := testResolver(t)
	body := `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:6.0,
v0.m4s
#EXTINF:6.006,
v1.m4s
#EXTINF:5.5,
v2.m4s
#EXT-X-ENDLIST
`
	tl, err := ParseHLS(r, "hls/media.m3u8", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if tl.Format != "hls" || len(tl.Streams) != 1 {
		t.Fatalf("timeline shape: %+v", tl)
	}
	segs := tl.Streams[0].Segments
	if len(segs) != 3 {
		t.Fatalf("segments=%d", len(segs))
	}
	if !segs[1].Start.Equal(model.MustRational(6, 1)) {
		t.Fatalf("seg1 start=%s", segs[1].Start)
	}
	if !segs[2].Start.Equal(model.MustRational(12006, 1000)) {
		t.Fatalf("seg2 start=%s", segs[2].Start)
	}
	if !tl.Duration.Equal(model.MustRational(17506, 1000)) {
		t.Fatalf("duration=%s", tl.Duration)
	}
}

func TestParseHLSRejectsNegativeAndOverflow(t *testing.T) {
	r := testResolver(t)
	for _, body := range []string{
		"#EXTM3U\n#EXTINF:-1,\na.m4s\n",
		"#EXTM3U\n#EXTINF:99999999999999999999999,\na.m4s\n",
		"#EXTM3U\n#EXTINF:6,\n../escape.m4s\n",
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n",
	} {
		if _, err := ParseHLS(r, "x.m3u8", []byte(body)); err == nil {
			t.Errorf("accepted bad playlist:\n%s", body)
		}
	}
}

func TestParseHLSByteRange(t *testing.T) {
	r := testResolver(t)
	body := "#EXTM3U\n#EXTINF:2,\n#EXT-X-BYTERANGE:100@0\nmain.mp4\n#EXTINF:2,\n#EXT-X-BYTERANGE:200@100\nmain.mp4\n"
	tl, err := ParseHLS(r, "b.m3u8", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	segs := tl.Streams[0].Segments
	if segs[1].ByteStart != 100 || segs[1].ByteEnd != 299 {
		t.Fatalf("byte range: %+v", segs[1])
	}
}

const testMPD = `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT12S">
  <Period id="p0">
    <AdaptationSet id="v" contentType="video" mimeType="video/mp4" timescale="90000">
      <SegmentTemplate timescale="90000" initialization="init-v.mp4" media="v-$Number$.m4s" startNumber="1">
        <SegmentTimeline>
          <S t="0" d="180000" r="2"/>
          <S d="90000"/>
        </SegmentTimeline>
      </SegmentTemplate>
      <Representation id="v0" bandwidth="800000" mimeType="video/mp4"/>
    </AdaptationSet>
    <AdaptationSet id="a" contentType="audio" mimeType="audio/mp4" timescale="48000">
      <SegmentTemplate timescale="48000" media="a-$Time$.m4s" initialization="init-a.mp4">
        <SegmentTimeline>
          <S t="0" d="96000" r="1"/>
        </SegmentTimeline>
      </SegmentTemplate>
      <Representation id="a0" bandwidth="128000" mimeType="audio/mp4"/>
    </AdaptationSet>
  </Period>
</MPD>`

func TestParseDASHTimeline(t *testing.T) {
	r := testResolver(t)
	tl, err := ParseDASH(r, "dash/manifest.mpd", []byte(testMPD))
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Streams) != 2 {
		t.Fatalf("streams=%d", len(tl.Streams))
	}
	video := tl.Streams[0]
	if video.Kind != "video" || len(video.Segments) != 4 {
		t.Fatalf("video stream: %+v", video)
	}
	if video.Segments[0].URI != "v-1.m4s" || video.Segments[3].Name != "v-4.m4s" {
		t.Fatalf("number expansion: %+v", video.Segments)
	}
	if !video.Segments[3].Start.Equal(model.MustRational(6, 1)) {
		t.Fatalf("video seg3 start=%s", video.Segments[3].Start)
	}
	audio := tl.Streams[1]
	if audio.Segments[0].URI != "a-0.m4s" || !audio.Segments[1].Start.Equal(model.MustRational(2, 1)) {
		t.Fatalf("time expansion: %+v", audio.Segments)
	}
	if !tl.Duration.Equal(model.MustRational(12, 1)) {
		t.Fatalf("duration=%s", tl.Duration)
	}
}

func TestParseDASHRejectsBadTimescaleAndBackwardsTimeline(t *testing.T) {
	r := testResolver(t)
	bad := strings.Replace(testMPD, `<SegmentTemplate timescale="90000"`, `<SegmentTemplate timescale="0"`, 1)
	if _, err := ParseDASH(r, "m.mpd", []byte(bad)); !errors.Is(err, model.ErrInvalidTimebase) {
		t.Fatalf("zero timescale: %v", err)
	}
	bad = strings.Replace(testMPD, `<S d="90000"/>`, `<S t="1" d="90000"/>`, 1)
	if _, err := ParseDASH(r, "m.mpd", []byte(bad)); err == nil {
		t.Fatal("backwards timeline accepted")
	}
}

func TestLoadDispatch(t *testing.T) {
	dir := t.TempDir()
	r, err := NewResolver(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.m3u8"), []byte("#EXTM3U\n#EXTINF:2,\na.m4s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl, err := Load(r, "a.m3u8")
	if err != nil || tl.Format != "hls" {
		t.Fatalf("Load hls: %v %+v", err, tl)
	}
	if _, err := Load(r, "../a.m3u8"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("traversal: %v", err)
	}
}
