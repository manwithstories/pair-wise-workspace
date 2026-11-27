// Package render draws offscreen review frames for the unified
// timeline and computes per-frame pixel checksums.
package render

import (
	"crypto/sha256"
	"image"
	"image/color"
	"image/draw"

	"github.com/disintegration/imaging"

	"cueforge/internal/clock"
	"cueforge/internal/model"
)

// Frame geometry for the offscreen review frames.
const (
	FrameWidth  = 320
	FrameHeight = 180
	rowHeight   = 4
	rowPitch    = 5
	marginX     = 10
)

var palette = []color.RGBA{
	{0x2b, 0x6c, 0xb0, 0xff},
	{0x2f, 0x85, 0x5a, 0xff},
	{0xb5, 0x53, 0x3c, 0xff},
	{0x6b, 0x46, 0xc1, 0xff},
	{0xb8, 0x8a, 0x2a, 0xff},
	{0x2c, 0x7a, 0x7b, 0xff},
}

// RenderFrame draws one review frame for a batch of events and
// returns its pixel checksum with the image. Output depends only on
// the inputs, so identical timelines yield identical checksums.
func RenderFrame(events []model.Event, frameIndex int, total clock.Time) ([32]byte, *image.NRGBA) {
	img := image.NewRGBA(image.Rect(0, 0, FrameWidth, FrameHeight))
	bg := color.RGBA{uint8(0x18 + frameIndex%8), 0x1a, 0x20, 0xff}
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)

	totalSec := total.Seconds()
	if totalSec <= 0 {
		totalSec = 1
	}
	span := float64(FrameWidth - 2*marginX)
	for i, ev := range events {
		y := 8 + i*rowPitch
		if y+rowHeight > FrameHeight-4 {
			break
		}
		x0 := marginX + int(ev.Start.Seconds()/totalSec*span)
		w := int(ev.Dur.Seconds() / totalSec * span)
		if w < 2 {
			w = 2
		}
		if x0+w > FrameWidth-marginX {
			w = FrameWidth - marginX - x0
		}
		if w <= 0 {
			continue
		}
		c := palette[ev.Track%len(palette)]
		draw.Draw(img, image.Rect(x0, y, x0+w, y+rowHeight),
			&image.Uniform{C: c}, image.Point{}, draw.Src)
	}
	// Offscreen transform via imaging for the review look.
	out := imaging.AdjustBrightness(img, -2)
	return sha256.Sum256(out.Pix), out
}

// SavePNG writes the frame as a PNG file.
func SavePNG(img image.Image, path string) error {
	return imaging.Save(img, path)
}
