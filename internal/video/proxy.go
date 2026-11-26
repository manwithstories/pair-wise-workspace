// Package video generates bounded proxy frames and adjusts JPEG quality
// by feedback toward the target bitrate.
package video

import (
	"bytes"
	"fmt"
	"image"

	"github.com/disintegration/imaging"
)

// MaxDim bounds the larger side of a proxy frame.
const MaxDim = 320

// Result holds one encoded proxy frame and its measurement reference.
type Result struct {
	Data  []byte      // encoded JPEG proxy
	Proxy image.Image // decoded proxy (what would be delivered)
	Ref   image.Image // downscaled original at the proxy dimensions
}

// EncodeProxy downscales src to fit MaxDim and JPEG-encodes it at quality.
func EncodeProxy(src image.Image, quality int) (Result, error) {
	ref := imaging.Fit(src, MaxDim, MaxDim, imaging.Lanczos)
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, ref, imaging.JPEG, imaging.JPEGQuality(quality)); err != nil {
		return Result{}, fmt.Errorf("proxy encode: %w", err)
	}
	proxy, err := imaging.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return Result{}, fmt.Errorf("proxy decode: %w", err)
	}
	return Result{Data: buf.Bytes(), Proxy: proxy, Ref: ref}, nil
}

// Controller adjusts JPEG quality so the running average bitrate tracks a
// per-frame byte budget. All decisions are deterministic.
type Controller struct {
	BudgetPerFrame float64
	Quality        int
}

// NewController derives a per-frame byte budget from the target bitrate
// and frame rate.
func NewController(targetBPS int64, fps float64) *Controller {
	return &Controller{BudgetPerFrame: float64(targetBPS) / 8 / fps, Quality: 80}
}

// Update adjusts Quality after framesDone frames produced producedBytes
// bytes in total.
func (c *Controller) Update(producedBytes, framesDone int64) {
	if producedBytes <= 0 || framesDone <= 0 {
		return
	}
	ratio := c.BudgetPerFrame * float64(framesDone) / float64(producedBytes)
	switch {
	case ratio > 1.10:
		c.Quality += 3
	case ratio > 1.02:
		c.Quality++
	case ratio < 0.90:
		c.Quality -= 3
	case ratio < 0.98:
		c.Quality--
	}
	if c.Quality < 20 {
		c.Quality = 20
	}
	if c.Quality > 95 {
		c.Quality = 95
	}
}
