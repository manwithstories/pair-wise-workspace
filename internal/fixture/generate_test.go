package fixture

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bq/internal/ffmpeg"
	"bq/internal/manifest"
)

func TestGenerateWritesManifestAndMedia(t *testing.T) {
	if _, _, err := ffmpeg.Binaries(); err != nil {
		t.Skipf("ffmpeg unavailable: %v", err)
	}
	opts := Options{
		Seed: 11, DurationS: 12, LossRate: 0.02, InjectWrap: true,
		InjectDup: true, InjectTrunc: true, PacketCadenceS: 1, GroupSize: 5,
	}
	m := Build(opts)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := Generate(ctx, m, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Manifest round-trips through the strict parser.
	parsed, errs := manifest.ParseFile(filepath.Join(dir, "manifest.json"))
	if len(errs) != 0 {
		t.Fatalf("re-parsed manifest invalid: %v", errs)
	}
	if parsed.Seed != 11 || len(parsed.Streams) != 4 {
		t.Fatal("round-trip lost data")
	}
	// Every declared media file exists on disk.
	for _, s := range parsed.Streams {
		p := filepath.Join(dir, "media", filepath.Base(s.File))
		info, err := os.Stat(p)
		if err != nil || info.Size() == 0 {
			t.Fatalf("media file missing/empty for stream %d: %v", s.ID, err)
		}
	}
	// Probe confirms codec and dimensions on a generated video.
	videoPath := filepath.Join(dir, "media", "video0.mp4")
	streams, err := ffmpeg.Probe(ctx, videoPath)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, si := range streams {
		if si.CodecType == "video" && si.CodecName == "h264" && si.Width == 96 {
			found = true
		}
	}
	if !found {
		t.Fatalf("generated video not probed as h264 96px: %+v", streams)
	}
}
