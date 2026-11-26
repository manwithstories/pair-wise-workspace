package hls

import (
	"path/filepath"
	"testing"

	"github.com/archive/tsrestore/internal/segment"
)

func fakeSegments(name string, n int, discAt map[int]bool) []*segment.Segment {
	out := make([]*segment.Segment, n)
	for i := 0; i < n; i++ {
		out[i] = &segment.Segment{
			ID:            segID(name, i),
			Index:         i,
			Rendition:     name,
			DurationSecs:  6.0,
			Discontinuity: discAt[i],
			KeyAligned:    true,
			SHA256:        "deadbeef",
		}
	}
	return out
}

func segID(name string, i int) string {
	return name + "-" + pad6(i)
}

func pad6(i int) string {
	const digits = "0123456789"
	b := make([]byte, 6)
	for j := 5; j >= 0; j-- {
		b[j] = digits[i%10]
		i /= 10
	}
	return string(b)
}

func TestWriteAndRoundTripManifests(t *testing.T) {
	variants := []Variant{
		{Name: "v1", Bandwidth: 1_200_000, Width: 854, Height: 480},
		{Name: "v2", Bandwidth: 2_400_000, Width: 1280, Height: 720},
	}
	segs := map[string][]*segment.Segment{
		"v1": fakeSegments("v1", 10, map[int]bool{3: true}),
		"v2": fakeSegments("v2", 10, nil),
	}
	dir := t.TempDir()
	masterPath, err := WriteManifests(dir, variants, segs)
	if err != nil {
		t.Fatal(err)
	}

	master, err := ReadMasterPlaylist(masterPath)
	if err != nil {
		t.Fatalf("master round trip: %v", err)
	}
	if len(master.Variants) != 2 {
		t.Fatalf("master variants = %d, want 2", len(master.Variants))
	}

	for _, v := range variants {
		pl, err := ReadMediaPlaylist(filepath.Join(dir, v.Name, "index.m3u8"))
		if err != nil {
			t.Fatalf("%s media round trip: %v", v.Name, err)
		}
		if pl.Count() != 10 {
			t.Fatalf("%s segment count = %d, want 10", v.Name, pl.Count())
		}
		if !pl.Closed {
			t.Fatalf("%s playlist should be closed (VOD ENDLIST)", v.Name)
		}
	}

	// The discontinuity flag must survive the round trip on v1's 4th segment.
	v1, err := ReadMediaPlaylist(filepath.Join(dir, "v1", "index.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	var disc int
	for _, s := range v1.Segments {
		if s != nil && s.Discontinuity {
			disc++
		}
	}
	if disc != 1 {
		t.Fatalf("discontinuity count = %d, want 1", disc)
	}
}

func TestMediaPlaylistDurations(t *testing.T) {
	segs := fakeSegments("v1", 4, nil)
	pl, err := BuildMediaPlaylist(segs)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Count() != 4 {
		t.Fatalf("count = %d", pl.Count())
	}
	first := pl.Segments[0]
	if first == nil || first.Duration != 6.0 {
		t.Fatalf("first segment duration = %v, want 6.0", first)
	}
}
