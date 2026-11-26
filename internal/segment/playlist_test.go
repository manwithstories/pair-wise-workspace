package segment

import (
	"strings"
	"testing"
)

func TestBuildPlaylistsRendersGapTagAligned(t *testing.T) {
	specs := []SegmentSpec{
		{Index: 0, Duration: 1.0, Gap: false, URI: "seg0.m4s"},
		{Index: 1, Duration: 1.0, Gap: true, URI: "seg1.m4s"},
	}
	master, variants, err := BuildPlaylists(specs, Tiers)
	if err != nil {
		t.Fatalf("BuildPlaylists: %v", err)
	}
	if len(variants) != len(Tiers) {
		t.Fatalf("variants = %d, want %d", len(variants), len(Tiers))
	}
	if !strings.Contains(master, "BANDWIDTH=180000") {
		t.Error("master playlist missing 180k variant")
	}
	if !strings.Contains(master, "#EXT-X-VERSION:7") {
		t.Error("master playlist must advertise version 7 when it contains a gap")
	}
	for _, v := range variants {
		if !strings.Contains(v.Content, "#EXT-X-VERSION:7") {
			t.Errorf("%dk: gapped playlist must be version 7", v.Tier.Kbps)
		}
		if strings.Count(v.Content, "#EXTINF") != 2 {
			t.Errorf("%dk: expected 2 EXTINF, got %d", v.Tier, strings.Count(v.Content, "#EXTINF"))
		}
		if strings.Count(v.Content, "#EXT-X-GAP") != 1 {
			t.Errorf("%dk: expected exactly 1 GAP tag, content:\n%s", v.Tier, v.Content)
		}
		// The GAP tag must apply to seg1 in every representation.
		idx := strings.Index(v.Content, "#EXT-X-GAP")
		seg1 := strings.Index(v.Content, "seg1.m4s")
		if idx < 0 || seg1 < 0 || idx > seg1 {
			t.Errorf("%dk: GAP tag must precede seg1 URI", v.Tier.Kbps)
		}
	}
}

func TestBuildPlaylistsNoGapKeepsDefaultVersion(t *testing.T) {
	specs := []SegmentSpec{
		{Index: 0, Duration: 1.0, URI: "seg0.m4s"},
	}
	master, variants, err := BuildPlaylists(specs, Tiers)
	if err != nil {
		t.Fatalf("BuildPlaylists: %v", err)
	}
	if strings.Contains(master, "#EXT-X-GAP") {
		t.Error("master must not carry a gap tag when no segment is gapped")
	}
	for _, v := range variants {
		if strings.Contains(v.Content, "#EXT-X-GAP") {
			t.Errorf("%dk: unexpected gap tag", v.Tier.Kbps)
		}
		if !strings.Contains(v.Content, "#EXT-X-VERSION:3") {
			t.Errorf("%dk: ungapped playlist should stay at version 3", v.Tier.Kbps)
		}
	}
}

func TestBuildPlaylistsSegmentCountAlignment(t *testing.T) {
	specs := make([]SegmentSpec, 4)
	for i := range specs {
		specs[i] = SegmentSpec{Index: i, Duration: 1.0, URI: "seg" + itoa(i) + ".m4s"}
	}
	_, variants, err := BuildPlaylists(specs, Tiers)
	if err != nil {
		t.Fatalf("BuildPlaylists: %v", err)
	}
	base := strings.Count(variants[0].Content, "#EXTINF")
	for _, v := range variants[1:] {
		if got := strings.Count(v.Content, "#EXTINF"); got != base {
			t.Errorf("variant %d has %d segments, want %d", v.Tier.Kbps, got, base)
		}
	}
}
