package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"review/internal/fault"
)

const testMPD = `<?xml version="1.0"?>
<MPD><Period><AdaptationSet mimeType="video/mp4">
  <Representation id="v1" bandwidth="800000" codecs="jpeg">
    <SegmentTemplate timescale="90000" initialization="init-$RepresentationID$.mp4"
      media="seg-$RepresentationID$-$Number%05d$.m4s" startNumber="1" duration="300000" segmentCount="4"/>
  </Representation>
  <Representation id="a1" bandwidth="128000" codecs="lpcm">
    <SegmentTemplate timescale="48000" initialization="init-$RepresentationID$.mp4"
      media="seg-$RepresentationID$-$Number%05d$.m4s" startNumber="1" duration="16000" segmentCount="4"/>
  </Representation>
</AdaptationSet></Period></MPD>`

func setupDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, id := range []string{"v1", "a1"} {
		if err := os.WriteFile(filepath.Join(dir, "init-"+id+".mp4"), []byte("init"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestParseDASHOK(t *testing.T) {
	dir := setupDir(t)
	p := filepath.Join(dir, "manifest.mpd")
	os.WriteFile(p, []byte(testMPD), 0o644)
	d, errs := ParseDASH(p)
	if len(errs) != 0 {
		t.Fatalf("unexpected faults: %v", errs)
	}
	if len(d.Representations) != 2 {
		t.Fatalf("reps=%d, want 2", len(d.Representations))
	}
	v := d.Representations[0]
	if v.Timescale != 90000 || len(v.MediaPaths) != 4 {
		t.Fatalf("rep=%+v", v)
	}
	want := filepath.Join(dir, "seg-v1-00002.m4s")
	if v.MediaPaths[1] != want {
		t.Fatalf("media[1]=%s, want %s", v.MediaPaths[1], want)
	}
}

func TestParseDASHUnknownCodec(t *testing.T) {
	dir := setupDir(t)
	bad := testMPD[:0] + ""
	_ = bad
	mpd := `<MPD><Period><AdaptationSet>
  <Representation id="x1" bandwidth="1" codecs="av01">
    <SegmentTemplate timescale="90000" initialization="init-x1.mp4" media="s-$Number$.m4s" startNumber="1" duration="1" segmentCount="1"/>
  </Representation></AdaptationSet></Period></MPD>`
	p := filepath.Join(dir, "bad.mpd")
	os.WriteFile(p, []byte(mpd), 0o644)
	_, errs := ParseDASH(p)
	f := fault.First(errs)
	if f == nil || f.Kind != fault.KindUnknownCodec {
		t.Fatalf("want unknown_codec, got %v", errs)
	}
	if f.Source != "rep:x1" || f.Recoverable {
		t.Fatalf("fault=%v", f)
	}
	if f.Offset <= 0 {
		t.Fatalf("offset=%d, want >0 (codecs attr position)", f.Offset)
	}
}

func TestParseDASHMissingInit(t *testing.T) {
	dir := t.TempDir() // 无 init 文件
	p := filepath.Join(dir, "m.mpd")
	os.WriteFile(p, []byte(testMPD), 0o644)
	_, errs := ParseDASH(p)
	f := fault.First(errs)
	if f == nil || f.Kind != fault.KindMissingInit {
		t.Fatalf("want missing_init, got %v", errs)
	}
	if f.Source != "rep:v1" {
		t.Fatalf("source=%s", f.Source)
	}
}

func TestResolvePathEscape(t *testing.T) {
	dir := t.TempDir()
	if _, ferr := Resolve(dir, "../escape.m4s", "", 0); ferr == nil || ferr.Kind != fault.KindPathEscape {
		t.Fatalf("want path_escape, got %v", ferr)
	}
	if _, ferr := Resolve(dir, "/abs/path.m4s", "", 0); ferr == nil {
		t.Fatal("want error for absolute path")
	}
	if _, ferr := Resolve(dir, "ok-$Number%03d$.m4s", "", 7); ferr != nil {
		t.Fatal(ferr)
	}
}

const testM3U8 = `#EXTM3U
#EXT-X-TIMESCALE:90000
#EXT-X-MAP:URI="init-h1.mp4"
#EXTINF:0.333,
seg-00001.m4s
#EXT-X-DISCONTINUITY
#EXTINF:0.333,
seg-00002.m4s
#EXTINF:0.333,
seg-00003.m4s
#EXT-X-DISCONTINUITY
#EXTINF:0.333,
seg-00004.m4s
#EXT-X-ENDLIST`

func TestParseHLSDiscontinuity(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "init-h1.mp4"), []byte("init"), 0o644)
	p := filepath.Join(dir, "index.m3u8")
	os.WriteFile(p, []byte(testM3U8), 0o644)
	h, errs := ParseHLS(p)
	if len(errs) != 0 {
		t.Fatalf("faults: %v", errs)
	}
	if h.DiscontinuityCount != 2 {
		t.Fatalf("discontinuities=%d, want 2", h.DiscontinuityCount)
	}
	if len(h.Segments) != 4 {
		t.Fatalf("segments=%d", len(h.Segments))
	}
	if !h.Segments[1].Discontinuity || !h.Segments[3].Discontinuity {
		t.Fatal("discontinuity flags on wrong segments")
	}
	if h.Segments[0].Discontinuity || h.Segments[2].Discontinuity {
		t.Fatal("unexpected discontinuity flags")
	}
	if h.Timescale != 90000 {
		t.Fatalf("timescale=%d", h.Timescale)
	}
}

func TestParseHLSMissingInit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "index.m3u8")
	os.WriteFile(p, []byte(testM3U8), 0o644)
	_, errs := ParseHLS(p)
	f := fault.First(errs)
	if f == nil || f.Kind != fault.KindMissingInit || f.Source != "hls:init" {
		t.Fatalf("got %v", errs)
	}
}
