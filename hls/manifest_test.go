package hls_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Eyevinn/hls-m3u8/m3u8"
	"github.com/fashionweek/mediad/internal/clock"
	"github.com/fashionweek/mediad/internal/hls"
	"github.com/fashionweek/mediad/internal/media"
)

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

func makeSpec(t *testing.T, v hls.Variant, n int) hls.PlaylistSpec {
	t.Helper()
	dur, err := clock.New(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	segs := make([]hls.SegmentSpec, n)
	for i := 0; i < n; i++ {
		segs[i] = hls.SegmentSpec{
			Seq: i, URI: filepath.Join(v.Name, "seg"+itoa(i)+".m4f"), Duration: dur,
		}
	}
	return hls.PlaylistSpec{Variant: v, Target: 2, Segments: segs}
}

type builtSet struct {
	master *m3u8.MasterPlaylist
	media  map[string]*m3u8.MediaPlaylist
	specs  map[string]hls.PlaylistSpec
}

func buildAll(t *testing.T, n int) builtSet {
	t.Helper()
	ladder := hls.DefaultLadder()
	mediaPl := map[string]*m3u8.MediaPlaylist{}
	specs := map[string]hls.PlaylistSpec{}
	ordered := make([]*m3u8.MediaPlaylist, len(ladder))
	for i, v := range ladder {
		spec := makeSpec(t, v, n)
		pl, err := hls.BuildMediaPlaylist(spec)
		if err != nil {
			t.Fatalf("%s build: %v", v.Name, err)
		}
		mediaPl[v.Playlist] = pl
		specs[v.Playlist] = spec
		ordered[i] = pl
	}
	master, err := hls.BuildMasterPlaylist(ladder, ordered)
	if err != nil {
		t.Fatal(err)
	}
	return builtSet{master: master, media: mediaPl, specs: specs}
}

func TestMediaPlaylistRoundTrip(t *testing.T) {
	for _, v := range hls.DefaultLadder() {
		spec := makeSpec(t, v, 4)
		pl, err := hls.BuildMediaPlaylist(spec)
		if err != nil {
			t.Fatalf("%s build: %v", v.Name, err)
		}
		data := append(pl.Encode().Bytes(), '\n')
		if err := hls.RoundTrip(data, spec); err != nil {
			t.Fatalf("%s round trip: %v", v.Name, err)
		}
	}
}

func TestMasterPlaylistLinksAllVariants(t *testing.T) {
	built := buildAll(t, 4)
	if len(built.master.Variants) != len(hls.DefaultLadder()) {
		t.Fatalf("variants = %d, want %d", len(built.master.Variants), len(hls.DefaultLadder()))
	}
	dir := t.TempDir()
	files, err := hls.RenderAll(dir, "master.m3u8", built.master, built.media)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := hls.ParseMaster(filepath.Join(dir, "master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Variants) != len(hls.DefaultLadder()) {
		t.Fatalf("parsed variants = %d, want %d", len(parsed.Variants), len(hls.DefaultLadder()))
	}
	for _, variant := range parsed.Variants {
		if _, err := os.Stat(filepath.Join(dir, variant.URI)); err != nil {
			t.Fatalf("media playlist %s missing: %v", variant.URI, err)
		}
	}
	if len(files) != len(hls.DefaultLadder())+1 {
		t.Fatalf("rendered files = %d, want %d", len(files), len(hls.DefaultLadder())+1)
	}
}

func TestRepeatedRenderIsByteIdentical(t *testing.T) {
	built := buildAll(t, 4)
	dir1, dir2 := t.TempDir(), t.TempDir()
	f1, err := hls.RenderAll(dir1, "master.m3u8", built.master, built.media)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := hls.RenderAll(dir2, "master.m3u8", built.master, built.media)
	if err != nil {
		t.Fatal(err)
	}
	for name := range f1 {
		if string(f1[name]) != string(f2[name]) {
			t.Fatalf("playlist %s differs across renders", name)
		}
	}
}

func TestEmptySegmentListRejected(t *testing.T) {
	v := hls.DefaultLadder()[0]
	_, err := hls.BuildMediaPlaylist(hls.PlaylistSpec{Variant: v, Target: 2})
	if !media.IsKind(err, media.KindBounds) {
		t.Fatalf("want bounds error, got %v", err)
	}
}
