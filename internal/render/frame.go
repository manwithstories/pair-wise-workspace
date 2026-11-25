// Package render turns decoded frames into an off-screen PNG sequence. Only
// JPEG/MJPEG essence is decoded; every other codec keeps its sample hash and
// returns a typed unsupported error.
package render

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

// DefaultMaxPixels caps decoded image area to prevent decompression bombs.
const DefaultMaxPixels = 4 << 20

// Options controls frame output.
type Options struct {
	// MaxPixels is the largest decoded width*height accepted; zero uses the
	// default.
	MaxPixels int
	// PNGCompression selects the encoder level; the zero value is
	// png.DefaultCompression, which is itself deterministic.
	PNGCompression int
}

// Result is the output of rendering one frame.
type Result struct {
	Frame    int
	Written  bool
	Path     string
	Sample   string // SHA-256 of the encoded sample bytes
	OutHash  string // SHA-256 of the PNG bytes written
	Width    int
	Height   int
	Rotation int
	Crop     model.Rect
	Err      error
}

// Frame renders one already-decoded JPEG sample, applying rotation first and
// then the resolved crop. The returned image is always newly allocated.
func Frame(sample []byte, desc model.Description, opts Options) (*image.RGBA, error) {
	maxPixels := opts.MaxPixels
	if maxPixels == 0 {
		maxPixels = DefaultMaxPixels
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(sample))
	if err != nil {
		return nil, fault.New(-1, 0, fault.KindCorrupt, true, "jpeg header unreadable: %v", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPixels {
		return nil, fault.New(-1, 0, fault.KindLimit, false,
			"jpeg dimensions %dx%d exceed pixel limit %d", cfg.Width, cfg.Height, maxPixels)
	}
	img, err := jpeg.Decode(bytes.NewReader(sample))
	if err != nil {
		return nil, fault.New(-1, 0, fault.KindCorrupt, true, "jpeg decode failed: %v", err)
	}
	rgba, ok := img.(*image.RGBA)
	if !ok {
		b := img.Bounds()
		rgba = image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
	}
	// Rotation is applied before cropping so crop insets refer to the final
	// orientation, as the delivery spec requires.
	rotated, err := rotate(rgba, desc.Rotation)
	if err != nil {
		return nil, err
	}
	cropped, err := crop(rotated, desc.Crop)
	if err != nil {
		return nil, err
	}
	return cropped, nil
}

// rotate turns an image clockwise by 0, 90, 180 or 270 degrees.
func rotate(src *image.RGBA, deg int) (*image.RGBA, error) {
	deg %= 360
	if deg < 0 {
		deg += 360
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	switch deg {
	case 0:
		return src, nil
	case 90:
		dst := image.NewRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				dst.SetRGBA(h-1-y, x, src.RGBAAt(x, y))
			}
		}
		return dst, nil
	case 180:
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				dst.SetRGBA(w-1-x, h-1-y, src.RGBAAt(x, y))
			}
		}
		return dst, nil
	case 270:
		dst := image.NewRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				dst.SetRGBA(y, w-1-x, src.RGBAAt(x, y))
			}
		}
		return dst, nil
	}
	return nil, fault.New(-1, 0, fault.KindMetadata, false,
		"rotation %d is not a quarter turn", deg)
}

// crop applies inset margins to an image.
func crop(src *image.RGBA, r model.Rect) (*image.RGBA, error) {
	if r.Empty() {
		return src, nil
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	nw, nh := w-r.Left-r.Right, h-r.Top-r.Bottom
	if nw <= 0 || nh <= 0 || r.Left < 0 || r.Top < 0 {
		return nil, fault.New(-1, 0, fault.KindMetadata, false,
			"crop %s leaves no pixels of %dx%d", r, w, h)
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		copy(dst.Pix[y*dst.Stride:(y*dst.Stride)+nw*4],
			src.Pix[(y+r.Top)*src.Stride+r.Left*4:])
	}
	return dst, nil
}

// EncodePNG writes img to buf with deterministic settings.
func EncodePNG(img image.Image, compression int) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.CompressionLevel(compression)}
	if compression == 0 {
		enc.CompressionLevel = png.DefaultCompression
	}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("render: png encode: %w", err)
	}
	return buf.Bytes(), nil
}

// WriteFrame renders a single sample and writes the PNG into dir using a
// deterministic file name. Non-decodable codecs produce a Result carrying the
// sample hash and an unsupported fault, without touching the filesystem.
func WriteFrame(dir string, frameNo int, sample []byte, codec model.Codec, desc model.Description, opts Options) Result {
	res := Result{Frame: frameNo, Rotation: desc.Rotation, Crop: desc.Crop}
	sum := sha256.Sum256(sample)
	res.Sample = fmt.Sprintf("%x", sum)
	if !codec.Decodable() {
		res.Err = fault.New(-1, 0, fault.KindUnsupported, true,
			"codec %s cannot be decoded to pixels; sample hash %s retained", codec, res.Sample)
		return res
	}
	img, err := Frame(sample, desc, opts)
	if err != nil {
		if fe, ok := fault.As(err); ok {
			res.Err = fe
		} else {
			res.Err = fault.Wrap(-1, 0, fault.KindCorrupt, true, err)
		}
		return res
	}
	pngBytes, err := EncodePNG(img, opts.PNGCompression)
	if err != nil {
		res.Err = fault.Wrap(-1, 0, fault.KindCorrupt, false, err)
		return res
	}
	name := fmt.Sprintf("frame_%04d.png", frameNo)
	if err := writeFileAtomic(filepath.Join(dir, name), pngBytes); err != nil {
		res.Err = fault.Wrap(-1, 0, fault.KindCorrupt, false, err)
		return res
	}
	outSum := sha256.Sum256(pngBytes)
	res.OutHash = fmt.Sprintf("%x", outSum)
	res.Path = name
	res.Width = img.Bounds().Dx()
	res.Height = img.Bounds().Dy()
	res.Written = true
	return res
}

// writeFileAtomic writes to a temporary file in the same directory and renames
// it into place so an interrupted run never leaves a partial frame behind and
// never overwrites a good frame with half a file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("render: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".relay-frame-*")
	if err != nil {
		return fmt.Errorf("render: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best effort cleanup; the rename below removes the need on success.
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("render: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("render: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("render: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("render: rename to %s: %w", path, err)
	}
	return nil
}
