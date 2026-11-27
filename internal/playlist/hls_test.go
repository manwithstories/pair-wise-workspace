package playlist

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMediaPlaylistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	segs := []Segment{
		{URI: "seg0.ts", DurationTicks: 180000},
		{URI: "seg1.ts", DurationTicks: 180000},
		{URI: "seg2.ts", DurationTicks: 90000},
	}
	if err := WriteMediaPlaylist(dir, segs); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMediaSegments(filepath.Join(dir, "playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("segments %v", got)
	}
	for i, s := range got {
		if s.URI != segs[i].URI {
			t.Fatalf("uri %d = %s", i, s.URI)
		}
		// EXTINF is millisecond-quantized; allow 1 ms of quantization.
		diff := s.DurationTicks - segs[i].DurationTicks
		if diff < -90 || diff > 90 {
			t.Fatalf("duration %d drift %d ticks", i, diff)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "playlist.m3u8"))
	if string(raw[len(raw)-20:]) == "" || !contains(string(raw), "#EXT-X-ENDLIST") {
		t.Fatal("playlist not closed")
	}
}

func TestWriteMaster(t *testing.T) {
	dir := t.TempDir()
	err := WriteMaster(dir, []Variant{
		{Name: "1080p", URI: "1080p/playlist.m3u8", Bandwidth: 2200000, Resolution: "1920x1080", Codecs: "avc1.640028,mp4a.40.2"},
		{Name: "720p", URI: "720p/playlist.m3u8", Bandwidth: 900000, Resolution: "1280x720", Codecs: "avc1.4d401f,mp4a.40.2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{"#EXTM3U", "BANDWIDTH=2200000", "1080p/playlist.m3u8", "720p/playlist.m3u8"} {
		if !contains(s, want) {
			t.Fatalf("master missing %q:\n%s", want, s)
		}
	}
}

func TestEmptySegmentsRejected(t *testing.T) {
	if err := WriteMediaPlaylist(t.TempDir(), nil); err == nil {
		t.Fatal("empty segment list must be rejected")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
