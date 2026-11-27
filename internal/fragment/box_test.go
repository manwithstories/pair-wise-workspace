package fragment

import (
	"os"
	"path/filepath"
	"testing"
)

// 验证 init 文件盒子布局：ftyp + moov(rinf)。
func TestInitBoxLayout(t *testing.T) {
	p := filepath.Join(t.TempDir(), "init.mp4")
	if err := WriteInit(p, &InitInfo{Codec: "jpeg", Timescale: 90000, Width: 160, Height: 90, FrameDuration: 3000}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	boxes, ferr := walkBoxes(raw)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if len(boxes) != 2 || boxes[0].typ != "ftyp" || boxes[1].typ != "moov" {
		t.Fatalf("boxes=%v", boxes)
	}
	inner, _ := walkBoxes(boxes[1].payload)
	if len(inner) != 1 || inner[0].typ != "rinf" {
		t.Fatalf("inner=%v", inner)
	}
}
