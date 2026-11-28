package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"presspack/internal/core"
)

func TestWriteAndReadMaster(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.m3u8")
	variants := []VariantInfo{
		{Name: "v400", PlaylistFile: "index.m3u8", Bandwidth: 400000, Width: 640, Height: 360, Codecs: "avc1.4d401f", FrameRate: 25},
		{Name: "v800", PlaylistFile: "index.m3u8", Bandwidth: 800000, Width: 640, Height: 360, Codecs: "avc1.4d401f", FrameRate: 25},
	}
	subs := &SubtitleInfo{GroupID: "subs", Name: "English", Language: "en", URI: "subs/index.m3u8"}
	if err := WriteMaster(path, variants, subs, []string{"cam3 truncated_tail @128"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{"#EXTM3U", "BANDWIDTH=400000", "BANDWIDTH=800000",
		"v400/index.m3u8", "v800/index.m3u8", "TYPE=SUBTITLES", `URI="subs/index.m3u8"`} {
		if !strings.Contains(s, want) {
			t.Errorf("master missing %q:\n%s", want, s)
		}
	}
}

func TestSubtitlePlaylistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subs.m3u8")
	names := []string{"seg_00000.vtt", "seg_00001.vtt", "seg_00002.vtt"}
	durs := []core.Time{core.FromMillis(2000), core.FromMillis(2000), core.FromMillis(1500)}
	if err := WriteSubtitlePlaylist(path, names, durs); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSegmentDurations(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("durations = %v", got)
	}
	for i, want := range []int64{2000, 2000, 1500} {
		if got[i].Millis() != want {
			t.Errorf("seg %d = %d ms, want %d", i, got[i].Millis(), want)
		}
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "#EXT-X-ENDLIST") {
		t.Error("VOD playlist must be closed")
	}
}

func TestReadSegmentDurationsRejectsMaster(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.m3u8")
	if err := WriteMaster(path, []VariantInfo{{Name: "v", PlaylistFile: "i.m3u8", Bandwidth: 1}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSegmentDurations(path); err == nil {
		t.Error("master playlist must be rejected as media playlist")
	}
}

func TestAtomicWriteNoTempLeft(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.m3u8")
	if err := atomicWrite(path, []byte("#EXTM3U\n")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "x.m3u8" {
		t.Errorf("temp file leaked: %v", entries)
	}
}
