package manifest

import (
	"fmt"
	"strings"
	"testing"

	"avcore/internal/clock"
	"avcore/internal/state"
)

func rec(variant, cut int, path string) state.SegmentRecord {
	return state.SegmentRecord{
		Variant: variant, CutIndex: cut, Path: path, SHA256: "h" + path,
		StartTicks: int64(cut) * clock.TicksPerSecond,
		EndTicks:   int64(cut+1) * clock.TicksPerSecond,
	}
}

func TestMediaPlaylistWithPartials(t *testing.T) {
	b := NewBuilder()
	records := []state.SegmentRecord{rec(0, 0, "v0_000.m4s"), rec(0, 1, "v0_001.m4s")}
	spec := VariantSpec{Name: "low", Bandwidth: 450000, Width: 320, Height: 240}
	out, err := b.MediaPlaylist(records, spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#EXTM3U",
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-PART-INF:PART-TARGET=0.500",
		"#EXT-X-PART:DURATION=0.500,INDEPENDENT=YES,URI=\"v0_000.part0.m4s\"",
		"#EXT-X-PART:DURATION=0.500,URI=\"v0_000.part1.m4s\"",
		"v0_000.m4s",
		"#EXT-X-ENDLIST",
		"#EXT-X-INDEPENDENT-SEGMENTS",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("playlist missing %q\n---\n%s", want, out)
		}
	}
	// Partials must be emitted before their full segment's EXTINF (HLS order).
	partPos := strings.Index(out, "#EXT-X-PART:")
	extinfPos := strings.Index(out, "#EXTINF")
	if partPos < 0 || extinfPos < 0 || partPos > extinfPos {
		t.Error("partial segments must precede the first EXTINF")
	}
	// Each full segment must carry exactly two partials.
	if got := strings.Count(out, "#EXT-X-PART:"); got != 4 {
		t.Errorf("partial count = %d, want 4", got)
	}
}

func TestMediaPlaylistRetainsAllVODPartials(t *testing.T) {
	// Eight full segments must keep all sixteen partials; the hls-m3u8 live
	// window would otherwise discard everything but the last three segments.
	b := NewBuilder()
	records := make([]state.SegmentRecord, 8)
	for i := range records {
		records[i] = rec(0, i, fmt.Sprintf("v0_%03d.m4s", i))
	}
	out, err := b.MediaPlaylist(records, VariantSpec{Name: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out, "#EXT-X-PART:"); got != 16 {
		t.Errorf("VOD partial count = %d, want 16 (all segments retained)", got)
	}
	if !strings.Contains(out, "v0_000.part0.m4s") {
		t.Error("first segment's partials must survive in a VOD playlist")
	}
	// Every full segment URI must still be present.
	for i := 0; i < 8; i++ {
		uri := fmt.Sprintf("v0_%03d.m4s", i)
		if !strings.Contains(out, uri) {
			t.Errorf("full segment %s missing", uri)
		}
	}
	// Verify interleaving: partials of segment N precede segment N's EXTINF,
	// which in turn precedes segment N+1's partials.
	firstPart := strings.Index(out, "v0_000.part0")
	firstFull := strings.Index(out, "v0_000.m4s")
	secondPart := strings.Index(out, "v0_001.part0")
	if !(firstPart < firstFull && firstFull < secondPart) {
		t.Error("partial/full segment ordering is not correctly interleaved")
	}
}

func TestMediaPlaylistRejectsEmpty(t *testing.T) {
	if _, err := NewBuilder().MediaPlaylist(nil, VariantSpec{}); err == nil {
		t.Error("empty records must fail")
	}
}

func TestMasterPlaylistVariantsAndSubtitles(t *testing.T) {
	b := NewBuilder()
	variants := []VariantSpec{
		{Name: "low", Bandwidth: 450000, Width: 320, Height: 240, FrameRate: 30, Codecs: "avc1.42c00d"},
		{Name: "high", Bandwidth: 900000, Width: 640, Height: 480, FrameRate: 30, Codecs: "avc1.42c01e"},
	}
	cap := &CaptionDoc{Language: "zh", Name: "中文", URI: "subs/index.m3u8"}
	out, err := b.MasterPlaylist(variants, []string{"v0/index.m3u8", "v1/index.m3u8"}, cap)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#EXT-X-MEDIA:TYPE=SUBTITLES",
		`GROUP-ID="subs"`,
		`URI="subs/index.m3u8"`,
		"BANDWIDTH=450000",
		"BANDWIDTH=900000",
		"RESOLUTION=320x240",
		"v0/index.m3u8",
		"v1/index.m3u8",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("master missing %q\n---\n%s", want, out)
		}
	}
}

func TestMasterPlaylistMismatch(t *testing.T) {
	_, err := NewBuilder().MasterPlaylist(
		[]VariantSpec{{Name: "a"}}, []string{"a.m3u8", "b.m3u8"}, nil)
	if err == nil {
		t.Error("variant/URI count mismatch must fail")
	}
}

func TestCaptionPlaylist(t *testing.T) {
	records := []state.CaptionRecord{
		{CutIndex: 0, Path: "subs_000.vtt", SHA256: "a"},
		{CutIndex: 1, Path: "subs_001.vtt", SHA256: "b"},
	}
	out, err := NewBuilder().CaptionPlaylist(records)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "subs_000.vtt") || !strings.Contains(out, "subs_001.vtt") {
		t.Errorf("caption URIs missing: %s", out)
	}
	if !strings.Contains(out, "#EXT-X-ENDLIST") {
		t.Error("caption playlist must be closed VOD")
	}
}

func TestPlaylistDeterministic(t *testing.T) {
	b := NewBuilder()
	records := []state.SegmentRecord{rec(0, 0, "v0_000.m4s"), rec(0, 1, "v0_001.m4s")}
	spec := VariantSpec{Name: "low", Bandwidth: 450000}
	a, _ := b.MediaPlaylist(records, spec)
	c, _ := b.MediaPlaylist(records, spec)
	if a != c {
		t.Error("identical committed records must render identical playlists")
	}
}

func TestStripExt(t *testing.T) {
	if got := stripExt("dir/v0_000.m4s"); got != "dir/v0_000" {
		t.Errorf("stripExt = %q", got)
	}
}
