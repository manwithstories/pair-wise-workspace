package manifest

import (
	"strings"
	"testing"
)

func testRenditions() []Rendition {
	mk := func(id, file string, bw uint32) Rendition {
		r := Rendition{ID: id, Playlist: file, Bandwidth: bw, Resolution: "640x360", FrameRate: 30}
		for i := uint64(1); i <= 3; i++ {
			r.Segments = append(r.Segments, Segment{
				Index: i, URI: "seg-" + id + "-00" + string(rune('0'+i)) + ".m4s",
				Start90k: int64((i - 1) * 180000), Duration90k: 180000,
			})
		}
		r.Segments[1].Discontinuity = true
		r.Segments[2].Gap = true
		return r
	}
	return []Rendition{mk("high", "high.m3u8", 5_000_000), mk("mid", "mid.m3u8", 2_000_000), mk("low", "low.m3u8", 800_000)}
}

func TestBuildMasterAndMediaPlaylists(t *testing.T) {
	b, err := Build("master.m3u8", testRenditions())
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Files) != 4 {
		t.Fatalf("files=%d want master+3", len(b.Files))
	}
	master := string(b.Files["master.m3u8"])
	for _, name := range []string{"high.m3u8", "mid.m3u8", "low.m3u8"} {
		if !strings.Contains(master, name) {
			t.Fatalf("master missing %s:\n%s", name, master)
		}
		pl := string(b.Files[name])
		if !strings.Contains(pl, "#EXTM3U") || !strings.Contains(pl, "#EXT-X-ENDLIST") {
			t.Fatalf("%s is not VOD playlist:\n%s", name, pl)
		}
		if strings.Count(pl, "#EXTINF:") != 3 {
			t.Fatalf("%s segment count wrong:\n%s", name, pl)
		}
		if !strings.Contains(pl, "#EXT-X-DISCONTINUITY") {
			t.Fatalf("%s missing discontinuity", name)
		}
		if !strings.Contains(pl, "#EXT-X-GAP") {
			t.Fatalf("%s missing gap", name)
		}
	}
}

func TestBuildRejectsUnequalCounts(t *testing.T) {
	rs := testRenditions()
	rs[2].Segments = rs[2].Segments[:2]
	if _, err := Build("master.m3u8", rs); err == nil {
		t.Fatal("unequal rendition segment counts accepted")
	}
}

func TestBuildRejectsBoundaryDriftBeyondOneTick(t *testing.T) {
	rs := testRenditions()
	rs[1].Segments[0].Start90k += 2
	if _, err := Build("master.m3u8", rs); err == nil {
		t.Fatal("two-tick boundary drift accepted")
	}
}

func TestBuildAllowsOneTickRounding(t *testing.T) {
	rs := testRenditions()
	rs[1].Segments[0].Start90k++
	if _, err := Build("master.m3u8", rs); err != nil {
		t.Fatalf("one-tick drift should be allowed: %v", err)
	}
}
