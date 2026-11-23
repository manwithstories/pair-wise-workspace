package render

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"image"
	"image/png"
	"io"

	"mediasim/internal/clock"
)

// FrameHash is the per-frame digest and the on-disk name it was written to.
type FrameHash struct {
	Index    int    `json:"index"`
	Ticks    int64  `json:"ticks"`
	SHA256   string `json:"sha256"`
	PNG      string `json:"png"`
	PNGBytes int    `json:"png_bytes"`
}

// EncodePNG writes the frame deterministically: the standard library PNG
// encoder with default compression is byte-stable for identical pixels.
func EncodePNG(w io.Writer, img *image.RGBA) error {
	if err := png.Encode(w, img); err != nil {
		return clock.NewError(clock.CatRender, clock.Span{}, clock.RecoveredNone, "png encode", err)
	}
	return nil
}

// HashImage hashes the pixel bytes, not the PNG container, so the digest is
// independent of encoder details and reproducible across machines.
func HashImage(img *image.RGBA) string {
	h := sha256.New()
	var hdr [16]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(img.Rect.Dx()))
	binary.BigEndian.PutUint32(hdr[4:8], uint32(img.Rect.Dy()))
	binary.BigEndian.PutUint32(hdr[8:12], uint32(img.Stride))
	binary.BigEndian.PutUint32(hdr[12:16], 4) // bytes per pixel
	h.Write(hdr[:])
	h.Write(img.Pix)
	return hex.EncodeToString(h.Sum(nil))
}

// HashPNG hashes the encoded container, for checking on-disk reproducibility.
func HashPNG(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
