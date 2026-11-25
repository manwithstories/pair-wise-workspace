package archivecheck_test

import (
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediaarchive/internal/clock"
	"mediaarchive/internal/fault"
	"mediaarchive/internal/ingest"
	"mediaarchive/internal/render"
	"mediaarchive/internal/timeline"
)

func mkRenderPacket(no int, track string, seq int64, start, dur clock.Ticks, hash string) ingest.Packet {
	return ingest.Packet{
		No: no, TrackID: track, Codec: "opus", ExtSeq: seq,
		Start: start, Duration: dur, Hash: hash,
		Source: ingest.SourceRef{Path: "fixtures/src.rtprec", Offset: int64(100 + no*20), Length: 16},
	}
}

func TestRenderPNGFramesAndPerFrameProvenance(t *testing.T) {
	packets := []ingest.Packet{
		mkRenderPacket(1, "track-1-aac", 1, 0, 1200, "aaaa"),
		mkRenderPacket(2, "track-1-aac", 2, 1200, 1200, "bbbb"),
		mkRenderPacket(3, "track-2-opus", 1, 0, 1000, "cccc"),
		mkRenderPacket(4, "track-2-opus", 2, 1000, 1000, "dddd"),
		mkRenderPacket(5, "track-2-opus", 4, 3000, 1000, "eeee"),
	}
	st := timeline.NewState()
	st.Holes = []fault.Hole{{Track: "track-2-opus", FirstMissing: 3, LastMissing: 3, StartTicks: 2000, EndTicks: 3000}}

	dir := t.TempDir()
	opt := render.Options{Out: dir, Width: 96, Height: 54, Interval: 2000, Profile: "test", Seed: 7, Events: 5, Tracks: 2, ParamsHash: "p", StateHash: "s"}
	manifest, framesHash, err := render.Export(packets, st, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Frames) != 2 {
		t.Fatalf("frames=%d want 2", len(manifest.Frames))
	}
	if framesHash == "" || manifest.Frames[0].PNGSHA256 == "" {
		t.Fatal("missing frame hashes")
	}

	pngPath := filepath.Join(dir, manifest.Frames[0].PNGPath)
	f, err := os.Open(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		t.Fatalf("PNG not decodable: %v", err)
	}
	if img.Bounds().Dx() != 96 || img.Bounds().Dy() != 54 {
		t.Fatalf("png bounds=%v", img.Bounds())
	}

	sourcesPath := filepath.Join(dir, manifest.Frames[0].SourceManifestPath)
	b, err := os.ReadFile(sourcesPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{"track-1-aac", "packet_no", "source_path", "offset", "sha256"} {
		if !strings.Contains(text, want) {
			t.Fatalf("source manifest missing %q:\n%s", want, text)
		}
	}
}

func TestRenderDeterministicHashAcrossDirectories(t *testing.T) {
	packets := []ingest.Packet{
		mkRenderPacket(1, "t", 1, 0, 1000, "aa"),
		mkRenderPacket(2, "t", 2, 1000, 1000, "bb"),
	}
	st := timeline.NewState()
	opt := func(dir string) render.Options {
		return render.Options{Out: dir, Width: 64, Height: 36, Interval: 1000, Seed: 41, Events: 2, Tracks: 1}
	}
	dir1, dir2 := t.TempDir(), t.TempDir()
	m1, h1, err := render.Export(packets, st, opt(dir1))
	if err != nil {
		t.Fatal(err)
	}
	m2, h2, err := render.Export(packets, st, opt(dir2))
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("frames hash differs: %s vs %s", h1, h2)
	}
	if len(m1.Frames) != len(m2.Frames) || m1.Frames[0].PNGSHA256 != m2.Frames[0].PNGSHA256 {
		t.Fatal("manifest frame hashes differ")
	}

	got, err := render.FileSHA256(filepath.Join(dir1, m1.Frames[0].PNGPath))
	if err != nil {
		t.Fatal(err)
	}
	if got != m1.Frames[0].PNGSHA256 {
		t.Fatalf("file hash %s != manifest png hash %s", got, m1.Frames[0].PNGSHA256)
	}
}
