package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDirMergesHLSAndDASH(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "hls/video0.ts", "x")
	write(t, dir, "index.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nhls/video0.ts\n#EXT-X-ENDLIST\n")
	write(t, dir, "dash/a0.m4s", "x")
	write(t, dir, "presentation.mpd", `<MPD><Period><AdaptationSet contentType="audio">
<Representation id="a"><SegmentTemplate timescale="1000" media="dash/a$Number$.m4s" startNumber="0">
<SegmentTimeline><S t="0" d="2000"/></SegmentTimeline></SegmentTemplate></Representation>
</AdaptationSet></Period></MPD>`)
	m, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("entries: %d", len(m.Entries))
	}
	kinds := map[string]bool{}
	for _, e := range m.Entries {
		kinds[string(e.Kind)] = true
	}
	if !kinds["video"] || !kinds["audio"] {
		t.Fatalf("missing merged tracks: %+v", kinds)
	}
	if m.Kind != "merged" {
		t.Fatalf("kind=%q, want merged", m.Kind)
	}
}

func TestLoadDirRejectsEmptyDirectory(t *testing.T) {
	if _, err := LoadDir(t.TempDir()); err == nil {
		t.Fatal("empty directory accepted")
	}
}

func TestLoadDirSingleManifestFallback(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "v.ts", "x")
	write(t, dir, "only.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nv.ts\n#EXT-X-ENDLIST\n")
	m, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 1 {
		t.Fatalf("entries: %d", len(m.Entries))
	}
}

func TestCaseDirectoryAcceptsFileInput(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "v.ts", "x")
	pl := write(t, dir, "single.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nv.ts\n#EXT-X-ENDLIST\n")
	if _, err := Load(pl); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "single.m3u8")); err != nil {
		t.Fatal(err)
	}
}
