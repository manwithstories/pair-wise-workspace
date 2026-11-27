package render

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"cueforge/internal/clock"
	"cueforge/internal/model"
)

func frameEvents() []model.Event {
	return []model.Event{
		{Track: 0, Start: clock.Must(0, 1000), Dur: clock.Must(500, 1000), Payload: "a"},
		{Track: 1, Start: clock.Must(250, 1000), Dur: clock.Must(500, 1000), Payload: "b"},
		{Track: 2, Start: clock.Must(750, 1000), Dur: clock.Must(250, 1000), Payload: "c"},
	}
}

func TestRenderDeterministic(t *testing.T) {
	total := clock.Must(1000, 1000)
	sum1, _ := RenderFrame(frameEvents(), 3, total)
	sum2, _ := RenderFrame(frameEvents(), 3, total)
	if sum1 != sum2 {
		t.Fatal("identical inputs produced different checksums")
	}
}

func TestRenderDependsOnInputs(t *testing.T) {
	total := clock.Must(1000, 1000)
	base, _ := RenderFrame(frameEvents(), 0, total)
	changed := frameEvents()
	changed[1].Payload = "different"
	// Payload does not affect pixels; move the event instead.
	changed[1].Start = clock.Must(900, 1000)
	other, _ := RenderFrame(changed, 0, total)
	if base == other {
		t.Fatal("different timelines produced identical checksums")
	}
	// Frame index changes the background, hence the checksum.
	_, _ = changed, other
	alt, _ := RenderFrame(frameEvents(), 1, total)
	if base == alt {
		t.Fatal("different frame indices produced identical checksums")
	}
}

func TestSavePNGDecodable(t *testing.T) {
	_, img := RenderFrame(frameEvents(), 0, clock.Must(1000, 1000))
	path := filepath.Join(t.TempDir(), "frame.png")
	if err := SavePNG(img, path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != FrameWidth || cfg.Height != FrameHeight {
		t.Fatalf("frame = %dx%d, want %dx%d", cfg.Width, cfg.Height, FrameWidth, FrameHeight)
	}
}
