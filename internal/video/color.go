// Package video normalizes source color descriptions to one sRGB output
// encoding and stamps the result with explicit PNG color metadata.
package video

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"

	"github.com/disintegration/imaging"

	"pairflow/internal/media"
)

// Space identifies a supported source color description. Unknown descriptions
// are rejected with a locatable error: nothing falls back silently.
type Space struct {
	Name      string
	Transfer  string
	Primaries string
	Gamma     float64
}

var (
	// SRGB is IEC 61966-2-1 with the piecewise sRGB transfer curve.
	SRGB = Space{Name: "srgb", Transfer: "sRGB IEC 61966-2-1", Primaries: "BT.709", Gamma: 0}
	// Rec709 is ITU-R BT.709 input referenced for a BT.1886 2.4 display.
	Rec709 = Space{Name: "rec709", Transfer: "ITU-R BT.709 / BT.1886", Primaries: "BT.709", Gamma: 2.4}
)

// ParseSpace resolves a declared color description. Accepted spellings are
// normalized so configs from different vendors agree, but an unknown value is
// an error naming the field.
func ParseSpace(raw, path, field string) (Space, *media.Error) {
	switch normalizeName(raw) {
	case "srgb":
		return SRGB, nil
	case "rec709", "bt709":
		return Rec709, nil
	default:
		return Space{}, media.Input("color.unknown_space",
			"unknown color space %q (supported: srgb, rec709)", raw).
			WithPath(path).WithField(field)
	}
}

func normalizeName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '.' || c == '-' || c == '_' || c == ' ' {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}

// linearize maps an encoded channel value in [0,1] to linear light.
func (s Space) linearize(v float64) float64 {
	if s.Name == "srgb" {
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return math.Pow(v, s.Gamma)
}

// encode is the inverse of linearize for the sRGB output encoding.
func encodeSRGB(v float64) float64 {
	if v <= 0.0031308 {
		return 12.92 * v
	}
	return 1.055*math.Pow(v, 1/2.4) - 0.055
}

// Normalize converts a decoded image from the declared source space into
// sRGB-encoded NRGBA. The conversion runs channel-wise through linear light so
// Rec.709 highlights do not crush when displayed as sRGB. Unknown spaces are
// rejected before any pixels are touched.
func Normalize(img image.Image, rawSpace, path string) (*image.NRGBA, *media.Error) {
	sp, err := ParseSpace(rawSpace, path, "color.space")
	if err != nil {
		return nil, err
	}
	if sp.Name == "srgb" {
		// Already in the target encoding; copy into NRGBA so later edits have a
		// known pixel layout.
		return imaging.Clone(img), nil
	}
	out := imaging.AdjustFunc(img, func(c color.NRGBA) color.NRGBA {
		r := sp.linearize(float64(c.R) / 255)
		g := sp.linearize(float64(c.G) / 255)
		b := sp.linearize(float64(c.B) / 255)
		return color.NRGBA{
			R: clamp8(encodeSRGB(r)),
			G: clamp8(encodeSRGB(g)),
			B: clamp8(encodeSRGB(b)),
			A: c.A,
		}
	})
	return out, nil
}

func clamp8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 1 {
		return 255
	}
	return uint8(math.Round(v * 255))
}

// Meta records the color description stamped into the PNG. SourceSpace names
// what the pixels were normalized from; Destination is always sRGB.
type Meta struct {
	SourceSpace string
	SourcePrim  string
	Destination string
}

// MetaFor builds the output metadata for a source description.
func MetaFor(rawSpace, path string) (Meta, *media.Error) {
	sp, err := ParseSpace(rawSpace, path, "color.space")
	if err != nil {
		return Meta{}, err
	}
	return Meta{SourceSpace: sp.Name, SourcePrim: sp.Primaries, Destination: "srgb"}, nil
}

// EncodePNG renders img to a PNG stamped with sRGB, gAMA, cHRM and a tEXt
// chunk naming the source description. The standard encoder cannot emit color
// chunks, so they are spliced in after IHDR, before any image data.
func EncodePNG(img image.Image, meta Meta) ([]byte, *media.Error) {
	var raw bytes.Buffer
	if err := png.Encode(&raw, img); err != nil {
		return nil, media.Wrap(err, media.ClassInternal, "color.png_encode", "cannot encode png")
	}
	chunks := make([]chunk, 0, 4)
	chunks = append(chunks, chunk{typ: "sRGB", data: []byte{0}}) // perceptual intent
	// gAMA encodes the sRGB transfer as 1/2.2 ~= 0.45455.
	chunks = append(chunks, chunk{typ: "gAMA", data: be32(45455)})
	// cHRM: sRGB/BT.709 primaries and D65 white point, in 1e-5 units.
	chunks = append(chunks, chunk{typ: "cHRM", data: chrmsRGB()})
	chunks = append(chunks, chunk{typ: "tEXt",
		data: []byte("pairflow:color_source\x00" + meta.SourceSpace + "->" + meta.Destination)})
	out, err := spliceChunks(raw.Bytes(), chunks)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// WritePNG writes an encoded PNG atomically enough for the store layer to sync:
// the file is created fresh at path.
func WritePNG(path string, img image.Image, meta Meta) *media.Error {
	data, err := EncodePNG(img, meta)
	if err != nil {
		return err.WithPath(path)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return media.Wrap(err, media.ClassInternal, "color.png_write", "cannot write png").WithPath(path)
	}
	return nil
}

// ReadColorMeta parses the color chunks of a PNG produced by this package.
func ReadColorMeta(data []byte) (Meta, *media.Error) {
	meta := Meta{}
	found := false
	for _, c := range chunksOf(data) {
		switch c.typ {
		case "sRGB":
			found = true
			meta.Destination = "srgb"
		case "tEXt":
			k, v, ok := bytes.Cut(c.data, []byte{0})
			if ok && string(k) == "pairflow:color_source" {
				src, dst, ok := bytes.Cut(v, []byte("->"))
				if ok {
					meta.SourceSpace = string(src)
					if meta.Destination == "" {
						meta.Destination = string(dst)
					}
				}
			}
		}
	}
	if !found {
		return Meta{}, media.Integrity("color.png_no_srgb", "png is missing the sRGB chunk")
	}
	if meta.SourceSpace == "" {
		return Meta{}, media.Integrity("color.png_no_source", "png lacks pairflow color provenance text")
	}
	return meta, nil
}

type chunk struct {
	typ  string
	data []byte
}

// chunksOf walks PNG chunks after the 8-byte signature.
func chunksOf(data []byte) []chunk {
	if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		return nil
	}
	var out []chunk
	off := 8
	for off+8 <= len(data) {
		n := int(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		if off+12+n > len(data) {
			break
		}
		out = append(out, chunk{typ: typ, data: data[off+8 : off+8+n]})
		off += 12 + n
		if typ == "IEND" {
			break
		}
	}
	return out
}

// spliceChunks inserts new chunks immediately after IHDR. Encoding is redone
// over the stream so CRCs remain valid.
func spliceChunks(data []byte, extras []chunk) ([]byte, *media.Error) {
	if len(data) < 8 {
		return nil, media.Integrity("color.png_short", "png shorter than signature")
	}
	out := make([]byte, 0, len(data)+len(extras)*24)
	out = append(out, data[:8]...)
	off := 8
	inserted := false
	for off+8 <= len(data) {
		n := int(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		end := off + 12 + n
		if end > len(data) {
			return nil, media.Integrity("color.png_truncated", "png chunk %q truncated", typ)
		}
		out = append(out, data[off:end]...)
		if typ == "IHDR" && !inserted {
			for _, c := range extras {
				enc, err := writeChunk(c)
				if err != nil {
					return nil, err
				}
				out = append(out, enc...)
			}
			inserted = true
		}
		off = end
		if typ == "IEND" {
			break
		}
	}
	if !inserted {
		return nil, media.Integrity("color.png_no_ihdr", "png lacks IHDR")
	}
	return out, nil
}

func writeChunk(c chunk) ([]byte, *media.Error) {
	if len(c.typ) != 4 {
		return nil, media.E(media.ClassInternal, "color.bad_chunk", "chunk type %q is not 4 bytes", c.typ)
	}
	buf := make([]byte, 0, 12+len(c.data))
	buf = append(buf, be32(uint32(len(c.data)))...)
	buf = append(buf, c.typ...)
	buf = append(buf, c.data...)
	sum := crc32.NewIEEE()
	_, _ = sum.Write([]byte(c.typ))
	_, _ = sum.Write(c.data)
	buf = append(buf, be32(sum.Sum32())...)
	return buf, nil
}

func be32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

// chrmsRGB writes the eight 32-bit fixed-point chromaticity values.
func chrmsRGB() []byte {
	vals := []uint32{31270, 32900, 64000, 33000, 30000, 60000, 15000, 6000}
	out := make([]byte, 32)
	for i, v := range vals {
		binary.BigEndian.PutUint32(out[i*4:], v)
	}
	return out
}

// Describe renders a short human-readable summary for terminal output.
func (m Meta) Describe() string {
	return fmt.Sprintf("source=%s->%s", m.SourceSpace, m.Destination)
}
