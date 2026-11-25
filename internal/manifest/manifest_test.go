package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/media"
)

// writeTree writes files under a fresh temp root and returns the root.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestHLSMediaPlaylist(t *testing.T) {
	root := writeTree(t, map[string]string{
		"media.m3u8": "#EXTM3U\n" +
			"#EXT-X-VERSION:3\n" +
			"#EXT-X-TARGETDURATION:4\n" +
			"#EXT-X-MEDIA-SEQUENCE:10\n" +
			"#EXTINF:4.004,\n" +
			"cam1/seg10.m4s\n" +
			"#EXTINF:4.004,\n" +
			"cam1/seg11.m4s\n" +
			"#EXT-X-ENDLIST\n",
	})
	m, err := ReadHLS(filepath.Join(root, "media.m3u8"), Options{Root: root})
	if err != nil {
		t.Fatalf("ReadHLS: %v", err)
	}
	if len(m.Segments) != 2 {
		t.Fatalf("segments=%d", len(m.Segments))
	}
	if m.Segments[0].Seq != 10 || m.Segments[1].Seq != 11 {
		t.Fatalf("sequence numbering wrong: %d,%d", m.Segments[0].Seq, m.Segments[1].Seq)
	}
	if got := m.Segments[0].Duration; got.Num != 1001 || got.Den != 250 {
		t.Fatalf("duration=%v", got)
	}
	if m.Segments[0].RelPath != "cam1/seg10.m4s" {
		t.Fatalf("relpath=%q", m.Segments[0].RelPath)
	}
}

func TestHLSMasterExpandsVariants(t *testing.T) {
	root := writeTree(t, map[string]string{
		"master.m3u8": "#EXTM3U\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080,CODECS=\"avc1.640028\"\n" +
			"cam1/main.m3u8\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,CODECS=\"avc1.4d401f\"\n" +
			"cam2/main.m3u8\n",
		"cam1/main.m3u8": "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\na.m4s\n#EXTINF:2.0,\nb.m4s\n",
		"cam2/main.m3u8": "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nc.m4s\n",
	})
	m, err := ReadHLS(filepath.Join(root, "master.m3u8"), Options{Root: root})
	if err != nil {
		t.Fatalf("ReadHLS master: %v", err)
	}
	if len(m.Renditions) != 2 || len(m.Segments) != 3 {
		t.Fatalf("renditions=%d segments=%d", len(m.Renditions), len(m.Segments))
	}
	if m.Segments[2].Rendition.Camera != "cam2" {
		t.Fatalf("rendition tagging failed: %+v", m.Segments[2].Rendition)
	}
	if m.Renditions[0].Codec != "avc1.640028" {
		t.Fatalf("codec=%q", m.Renditions[0].Codec)
	}
}

func TestHLSErrors(t *testing.T) {
	t.Run("missing extinf", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"m.m3u8": "#EXTM3U\n#EXT-X-TARGETDURATION:2\nno-duration.m4s\n",
		})
		_, err := ReadHLS(filepath.Join(root, "m.m3u8"), Options{Root: root})
		assertCode(t, err, media.CodeMissingEntry, 3)
	})
	t.Run("invalid duration", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"m.m3u8": "#EXTM3U\n#EXTINF:-1,\na.m4s\n",
		})
		_, err := ReadHLS(filepath.Join(root, "m.m3u8"), Options{Root: root})
		assertCode(t, err, media.CodeInvalidDuration, 2)
	})
	t.Run("duplicate sequence", func(t *testing.T) {
		segs := []Segment{
			{Seq: 1, Pos: media.SourcePos{File: "a", Line: 2}},
			{Seq: 1, Pos: media.SourcePos{File: "a", Line: 5}},
		}
		err := ValidateSegments(segs)
		assertCode(t, err, media.CodeDuplicateSeq, 5)
	})
	t.Run("path traversal", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"m.m3u8": "#EXTM3U\n#EXTINF:2.0,\n../../etc/passwd\n",
		})
		_, err := ReadHLS(filepath.Join(root, "m.m3u8"), Options{Root: root})
		assertCode(t, err, media.CodePathTraversal, 3)
	})
	t.Run("absolute url", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"m.m3u8": "#EXTM3U\n#EXTINF:2.0,\nhttps://example.com/a.m4s\n",
		})
		_, err := ReadHLS(filepath.Join(root, "m.m3u8"), Options{Root: root})
		assertCode(t, err, media.CodeURIOutOfRange, 3)
	})
}

func TestDASHSegmentTemplate(t *testing.T) {
	root := writeTree(t, map[string]string{
		"stream.mpd": `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT12S">
  <Period duration="PT12S">
    <AdaptationSet id="cam1" contentType="video" mimeType="video/mp4" codecs="avc1">
      <SegmentTemplate timescale="90000" duration="180000" startNumber="1"
        media="cam1/seg-$Number$.m4s" initialization="cam1/init.mp4"/>
      <Representation id="cam1" bandwidth="4000000" width="1280" height="720"/>
    </AdaptationSet>
  </Period>
</MPD>`,
	})
	m, err := ReadDASH(filepath.Join(root, "stream.mpd"), Options{Root: root})
	if err != nil {
		t.Fatalf("ReadDASH: %v", err)
	}
	if len(m.Segments) != 6 {
		t.Fatalf("expected 6 segments, got %d", len(m.Segments))
	}
	if m.Segments[0].RelPath != "cam1/seg-1.m4s" || m.Segments[5].RelPath != "cam1/seg-6.m4s" {
		t.Fatalf("template expansion wrong: %v", m.Segments)
	}
	if m.Segments[1].StartTicks != 180000 {
		t.Fatalf("start ticks=%d", m.Segments[1].StartTicks)
	}
}

func TestDASHMalformedIsolates(t *testing.T) {
	root := writeTree(t, map[string]string{
		"bad.mpd": "<MPD>\n  <Period>\n    <AdaptationSet>\n",
	})
	_, err := ReadDASH(filepath.Join(root, "bad.mpd"), Options{Root: root})
	if err == nil {
		t.Fatal("expected malformed MPD error")
	}
	if media.CodeOf(err) != media.CodeManifest {
		t.Fatalf("code=%s err=%v", media.CodeOf(err), err)
	}
	e, _ := media.AsError(err)
	if e.Pos.Line == 0 {
		t.Fatalf("expected a source line for the malformed MPD, got %+v", e.Pos)
	}
}

func assertCode(t *testing.T, err error, want media.Code, wantLine int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error, got nil", want)
	}
	if got := media.CodeOf(err); got != want {
		t.Fatalf("code=%s want %s (err=%v)", got, want, err)
	}
	e, _ := media.AsError(err)
	if wantLine > 0 && e.Pos.Line != wantLine {
		t.Fatalf("line=%d want %d (%v)", e.Pos.Line, wantLine, err)
	}
}
