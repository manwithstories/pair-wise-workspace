package render

import (
	"os"
	"testing"
)

func TestRenderSequenceDeterministic(t *testing.T) {
	dir := t.TempDir()
	// 源帧：用图样生成 JPEG
	var inputs []FrameInput
	for i := 0; i < 3; i++ {
		src, err := EncodeJPEG(MakeFrameImage(i, 160, 90), 80)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, FrameInput{Name: "f" + string(rune('0'+i)), Source: src, Seed: i})
	}
	frames1, digest1, err := RenderSequence(dir+"/a", inputs, 85)
	if err != nil {
		t.Fatal(err)
	}
	frames2, digest2, err := RenderSequence(dir+"/b", inputs, 85)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames1) != 3 {
		t.Fatalf("frames=%d", len(frames1))
	}
	for i := range frames1 {
		f := frames1[i]
		if f.Width != 160 || f.Height != 90 {
			t.Fatalf("dims=%dx%d", f.Width, f.Height)
		}
		if len(f.SHA256) != 64 {
			t.Fatalf("sha=%q", f.SHA256)
		}
		if f.SHA256 != frames2[i].SHA256 {
			t.Fatal("same input must give same sha256")
		}
		if _, err := os.Stat(f.Path); err != nil {
			t.Fatal("frame file missing")
		}
	}
	if digest1 != digest2 || len(digest1) != 64 {
		t.Fatalf("digest mismatch %q vs %q", digest1, digest2)
	}
}

func TestRenderCorruptSourcePlaceholder(t *testing.T) {
	dir := t.TempDir()
	inputs := []FrameInput{{Name: "bad", Source: []byte("not a jpeg"), Seed: 7}}
	frames, _, err := RenderSequence(dir, inputs, 85)
	if err != nil {
		t.Fatal(err)
	}
	if frames[0].Width != 160 || frames[0].Height != 90 {
		t.Fatalf("placeholder dims=%dx%d", frames[0].Width, frames[0].Height)
	}
}
