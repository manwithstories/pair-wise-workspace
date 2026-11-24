package render

import (
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"

	"mediarun/internal/clock"
	"mediarun/internal/model"
)

// draw paints a deterministic review raster: a timeline bar, a marker at
// the frame position and one stripe per folded event kind.
func draw(cfg Config, start clock.Tick, events []model.Event) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
	for y := 0; y < cfg.Height; y++ {
		for x := 0; x < cfg.Width; x++ {
			img.SetRGBA(x, y, palette[0])
		}
	}
	// Frame border in slate.
	border := palette[7]
	for x := 0; x < cfg.Width; x++ {
		img.SetRGBA(x, 0, border)
		img.SetRGBA(x, cfg.Height-1, border)
	}
	for y := 0; y < cfg.Height; y++ {
		img.SetRGBA(0, y, border)
		img.SetRGBA(cfg.Width-1, y, border)
	}
	// Timeline bar across the middle.
	mid := cfg.Height / 2
	barColor := palette[6]
	for x := 4; x < cfg.Width-4; x++ {
		img.SetRGBA(x, mid, barColor)
	}
	// Marker: the tick position is a stable function of the timestamp.
	pos := int(int64(start)/(int64(clock.TickRate)/4)) % (cfg.Width - 8)
	if pos < 0 {
		pos = -pos
	}
	marker := palette[1]
	for y := mid - 6; y <= mid+6; y++ {
		if y < 1 || y >= cfg.Height-1 {
			continue
		}
		img.SetRGBA(4+pos, y, marker)
	}
	// One horizontal stripe per event kind present at this tick.
	for i, ev := range events {
		y := 6 + i*3
		if y >= cfg.Height-4 {
			break
		}
		c := palette[1+(int(ev.Kind)%6)]
		for x := 8; x < cfg.Width-8; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// WriteTo persists the sequence into dir: frames/frame-XXX.png and
// manifest.jsonl. Only files inside dir are written.
func (s *Sequence) WriteTo(dir string) error {
	if len(s.Frames) == 0 {
		return fmt.Errorf("%w: empty sequence", model.ErrInvalid)
	}
	framesDir := filepath.Join(dir, "frames")
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		return err
	}
	for _, f := range s.Frames {
		name := fmt.Sprintf("frame-%03d.png", f.Index)
		if err := os.WriteFile(filepath.Join(framesDir, name), f.PNG, 0o644); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.jsonl"), s.Manifest, 0o644); err != nil {
		return err
	}
	return nil
}

// EncodePNG exposes the deterministic encoder for external verification.
func EncodePNG(w io.Writer, start clock.Tick, events []model.Event, cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	return png.Encode(w, draw(cfg, start, events))
}
