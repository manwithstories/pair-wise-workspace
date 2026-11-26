package video

import (
	"image"
	"image/color"
	"testing"

	"mediad/internal/metrics"
)

func gradient() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 640, 360))
	for y := 0; y < 360; y++ {
		for x := 0; x < 640; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	return img
}

func TestEncodeProxy(t *testing.T) {
	res, err := EncodeProxy(gradient(), 85)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Data) == 0 {
		t.Fatal("empty proxy data")
	}
	b := res.Proxy.Bounds()
	if b.Dx() > MaxDim || b.Dy() > MaxDim {
		t.Fatalf("proxy too large: %v", b)
	}
	if res.Ref.Bounds() != b {
		t.Fatalf("ref bounds %v != proxy %v", res.Ref.Bounds(), b)
	}
	if mse := metrics.MSE(res.Ref, res.Proxy); mse <= 0 {
		t.Fatalf("expected positive MSE, got %v", mse)
	}
	// higher quality must not be worse than lower quality
	lo, err := EncodeProxy(gradient(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.MSE(lo.Ref, lo.Proxy) < metrics.MSE(res.Ref, res.Proxy) {
		t.Fatal("quality 85 should beat quality 30")
	}
}

func TestControllerFeedback(t *testing.T) {
	c := NewController(1800000, 24)
	if c.BudgetPerFrame != 9375 {
		t.Fatalf("budget=%v", c.BudgetPerFrame)
	}
	q0 := c.Quality
	c.Update(100, 10) // way under budget -> quality up
	if c.Quality <= q0 {
		t.Fatalf("quality did not rise: %d", c.Quality)
	}
	c = NewController(1800000, 24)
	c.Update(1<<30, 10) // way over budget -> quality down
	if c.Quality >= q0 {
		t.Fatalf("quality did not drop: %d", c.Quality)
	}
	for i := 0; i < 100; i++ {
		c.Update(1<<30, 10)
	}
	if c.Quality != 20 {
		t.Fatalf("quality floor: %d", c.Quality)
	}
}
