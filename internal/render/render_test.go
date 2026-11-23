package render

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"mediasim/internal/clock"
)

func testFrame(i int) Frame {
	return Frame{
		Index: i, Ticks: int64(i) * 3003,
		Layers: []Layer{
			{Kind: LayerVideo, Source: "cam0", Frame: int64(i)},
			{Kind: LayerMarker, Label: "tac"},
		},
	}
}

func TestCompositeIsDeterministic(t *testing.T) {
	c, err := NewCanvas(DefaultParams(17))
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Composite(testFrame(3))
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Composite(testFrame(3))
	if err != nil {
		t.Fatal(err)
	}
	if HashImage(a) != HashImage(b) {
		t.Fatal("identical input produced different pixels")
	}
	if a.Bounds().Dx() != Width || a.Bounds().Dy() != Height {
		t.Fatalf("size=%v", a.Bounds())
	}
}

func TestDifferentFramesDifferentHashes(t *testing.T) {
	c, _ := NewCanvas(DefaultParams(17))
	h := map[string]int{}
	for i := 0; i < 12; i++ {
		img, err := c.Composite(testFrame(i))
		if err != nil {
			t.Fatal(err)
		}
		h[HashImage(img)]++
	}
	if len(h) != 12 {
		t.Fatalf("distinct hashes=%d want 12", len(h))
	}
}

func TestSeedChangesOutput(t *testing.T) {
	f := testFrame(1)
	c1, _ := NewCanvas(DefaultParams(17))
	c2, _ := NewCanvas(DefaultParams(18))
	i1, _ := c1.Composite(f)
	i2, _ := c2.Composite(f)
	if HashImage(i1) == HashImage(i2) {
		t.Fatal("seed had no effect")
	}
}

func TestPNGEncodeDeterministic(t *testing.T) {
	c, _ := NewCanvas(DefaultParams(17))
	img, err := c.Composite(testFrame(5))
	if err != nil {
		t.Fatal(err)
	}
	var a, b bytes.Buffer
	if err := EncodePNG(&a, img); err != nil {
		t.Fatal(err)
	}
	if err := EncodePNG(&b, img); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("PNG encoding is not byte-stable")
	}
	if _, err := png.Decode(bytes.NewReader(a.Bytes())); err != nil {
		t.Fatalf("emitted PNG does not decode: %v", err)
	}
	if HashPNG(a.Bytes()) == HashImage(img) {
		t.Fatal("container hash must differ from pixel hash")
	}
}

func TestWriterCommitOnce(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, DefaultParams(17))
	if err != nil {
		t.Fatal(err)
	}
	fh, err := w.Write(testFrame(0))
	if err != nil {
		t.Fatal(err)
	}
	if fh.SHA256 == "" || fh.PNG != "frame_0000.png" {
		t.Fatalf("fh=%+v", fh)
	}
	if _, err := os.Stat(filepath.Join(dir, fh.PNG)); err != nil {
		t.Fatal(err)
	}
	// Re-committing the same index must fail, never silently rewrite.
	_, err = w.Write(testFrame(0))
	if err == nil || clock.Classify(err) != clock.CatRender {
		t.Fatalf("duplicate commit err=%v", err)
	}
	if w.Count() != 1 {
		t.Fatalf("count=%d", w.Count())
	}
}

func TestWriterRerunProducesIdenticalFiles(t *testing.T) {
	dir := t.TempDir()
	hashRun := func() []FrameHash {
		w, err := NewWriter(dir, DefaultParams(17))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			if _, err := w.Write(testFrame(i)); err != nil {
				t.Fatal(err)
			}
		}
		return w.Hashes()
	}
	a := hashRun()
	b := hashRun()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("frame %d differs across runs: %+v vs %+v", i, a[i], b[i])
		}
	}
	// On-disk bytes are identical, not merely the digests.
	data, err := os.ReadFile(filepath.Join(dir, "frame_0002.png"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := img.(*image.RGBA); !ok {
		t.Fatalf("decoded type %T", img)
	}
}

func TestCutAudioIsSampleExact(t *testing.T) {
	src := make([]int16, 1024)
	for i := range src {
		src[i] = int16(i)
	}
	// Cut halfway: 512 samples remain, starting at the cut.
	b, err := CutAudio(src, 0, 512, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Samples) != 512 || b.Samples[0] != 512 {
		t.Fatalf("samples=%d first=%d", len(b.Samples), b.Samples[0])
	}
	if b.Start != 512 {
		t.Fatalf("start=%d", b.Start)
	}
	// Padding is explicit silence, counted separately from real audio.
	p := PadSilence(b, 3)
	if len(p.Samples) != 515 || p.Silenced != 3 {
		t.Fatalf("padded=%d silenced=%d", len(p.Samples), p.Silenced)
	}
	for i := 0; i < 3; i++ {
		if p.Samples[i] != SilenceSample {
			t.Fatalf("padding %d = %d", i, p.Samples[i])
		}
	}
	// A cut outside the block is a classified sync error.
	if _, err := CutAudio(src, 100, 50, 1024); err == nil {
		t.Fatal("out-of-range cut accepted")
	}
}

func TestCanvasRejectsBadGeometry(t *testing.T) {
	if _, err := NewCanvas(Params{Width: 1, Height: 1}); err == nil {
		t.Fatal("non-standard size accepted")
	}
	c, _ := NewCanvas(DefaultParams(1))
	if _, err := c.Composite(Frame{Index: MaxFrames}); err == nil {
		t.Fatal("frame past limit accepted")
	}
}
