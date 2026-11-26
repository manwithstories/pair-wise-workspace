package video

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"pairflow/internal/media"
)

func TestParseSpaceAcceptedSpellings(t *testing.T) {
	for _, raw := range []string{"srgb", "sRGB", "s-RGB", "REC.709", "bt709", "BT.709"} {
		if _, err := ParseSpace(raw, "cfg", "color.space"); err != nil {
			t.Fatalf("%q must be accepted: %v", raw, err)
		}
	}
}

func TestParseSpaceUnknownIsLocatable(t *testing.T) {
	_, err := ParseSpace("p3-d65", "clip.png", "video.space")
	if err == nil {
		t.Fatal("unknown space accepted")
	}
	if err.Class != media.ClassInput || err.Path != "clip.png" || err.Field != "video.space" {
		t.Fatalf("error not locatable: %+v / %s", err, err.Error())
	}
	if !contains(err.Error(), "p3-d65") {
		t.Fatalf("error must name the offending value: %s", err.Error())
	}
}

func TestNormalizeRec709ProducesDifferentEncoding(t *testing.T) {
	src := Pattern(PatternBars, "amber", SRGB, 0)
	rec := Pattern(PatternBars, "amber", Rec709, 0)
	// The same linear scene encoded in two transfers must yield different
	// 8-bit values.
	if sameImage(src, rec) {
		t.Fatal("rec709 and srgb encodings are identical; encoding test is vacuous")
	}
	back, err := Normalize(rec, "rec709", "clip")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	// Round-tripping Rec.709 -> sRGB should recover the sRGB render within
	// 8-bit quantization error (a couple of code values per channel).
	var worst int
	for y := 0; y < 180; y++ {
		for x := 0; x < 320; x++ {
			a, b := back.NRGBAAt(x, y), src.NRGBAAt(x, y)
			for _, d := range []int{abs(int(a.R) - int(b.R)), abs(int(a.G) - int(b.G)), abs(int(a.B) - int(b.B))} {
				if d > worst {
					worst = d
				}
			}
		}
	}
	if worst > 6 {
		t.Fatalf("rec709->srgb round trip drifted by %d code values (max 6)", worst)
	}
}

func TestNormalizeSRGBPassthrough(t *testing.T) {
	src := Pattern(PatternRings, "teal", SRGB, 5)
	out, err := Normalize(src, "srgb", "clip")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !sameImage(src, out) {
		t.Fatal("sRGB normalization must not alter pixels")
	}
}

func TestEncodePNGStampsColorMetadata(t *testing.T) {
	meta, err := MetaFor("rec709", "clip")
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	img, _ := Normalize(Pattern(PatternBars, "neutral", Rec709, 2), "rec709", "clip")
	data, err := EncodePNG(img, meta)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("png not decodable after chunk splice: %v", err)
	}
	got, err := ReadColorMeta(data)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if got.Destination != "srgb" || got.SourceSpace != "rec709" {
		t.Fatalf("metadata wrong: %+v", got)
	}
	// The chunk splice must not corrupt trailing image data; check CRCs by
	// re-encoding round trip already did a full parse, plus explicit sRGB.
	if !hasChunk(data, "sRGB") || !hasChunk(data, "gAMA") || !hasChunk(data, "cHRM") {
		t.Fatal("png missing color chunks")
	}
}

func TestReadColorMetaRejectsMissingSRGB(t *testing.T) {
	img := Pattern(PatternBars, "teal", SRGB, 0)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadColorMeta(buf.Bytes()); err == nil || err.Class != media.ClassIntegrity {
		t.Fatalf("missing sRGB chunk must be an integrity error, got %v", err)
	}
}

func TestValidatePatternRejectsUnknown(t *testing.T) {
	if err := ValidatePattern("noise", "teal", "p", "f"); err == nil || err.Class != media.ClassInput {
		t.Fatal("unknown pattern must be rejected")
	}
	if err := ValidatePattern("bars", "purple", "p", "f"); err == nil || err.Class != media.ClassInput {
		t.Fatal("unknown tint must be rejected")
	}
	if err := ValidatePattern("rings", "neutral", "p", "f"); err != nil {
		t.Fatalf("valid pattern rejected: %v", err)
	}
}

func TestBlendEndpoints(t *testing.T) {
	a := Pattern(PatternBars, "teal", SRGB, 0)
	b := Pattern(PatternRings, "amber", SRGB, 0)
	if !sameImage(Blend(a, b, 0), a) || !sameImage(Blend(a, b, 1), b) {
		t.Fatal("blend endpoints must return exact inputs")
	}
	mid := Blend(a, b, 0.5)
	if sameImage(mid, a) || sameImage(mid, b) {
		t.Fatal("midpoint blend equals an endpoint")
	}
	if mid.Bounds() != image.Rect(0, 0, 320, 180) {
		t.Fatalf("blend changed dimensions: %v", mid.Bounds())
	}
}

func sameImage(a, b image.Image) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			if color.NRGBAModel.Convert(a.At(x, y)) != color.NRGBAModel.Convert(b.At(x, y)) {
				return false
			}
		}
	}
	return true
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func hasChunk(data []byte, typ string) bool {
	for _, c := range chunksOf(data) {
		if c.typ == typ {
			return true
		}
	}
	return false
}
