package render_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"tsproof/internal/adapter"
	"tsproof/internal/clock"
	"tsproof/internal/mixer"
	"tsproof/internal/render"
)

// makeJPEG 生成最小合法 JPEG 块负载。
func makeJPEG(w, h int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	img.SetNRGBA(0, 0, color.NRGBA{R: 200, G: 100, B: 50, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 70}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func slot(seq int, pts int64, key, untrust bool, audioN int) mixer.Slot {
	frame := &adapter.Block{
		TrackID: 0x100, Index: seq, PTS27: pts, Key: key,
		Kind: adapter.KindImage, Payload: makeJPEG(64, 48),
	}
	kind := clock.FrameInter
	if key {
		kind = clock.FrameKey
	}
	s := mixer.Slot{At: clock.Tick(pts), Frame: frame, FrameKind: kind, Untrusted: untrust}
	for i := 0; i < audioN; i++ {
		s.Audio = append(s.Audio, &adapter.Block{
			TrackID: 0x101, Index: i, PTS27: pts, Key: true,
			Kind: adapter.KindAudio, Samples: 32 + i,
		})
	}
	return s
}

func TestRenderDeterministicAndManifest(t *testing.T) {
	dir := t.TempDir()
	r, err := render.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := slot(0, 27_000_000, true, false, 2)
	png1, m1, err := r.RenderSlot(s, 0)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	png2, m2, err := r.RenderSlot(s, 0)
	if err != nil {
		t.Fatalf("re-render: %v", err)
	}
	// 同槽位重复渲染必须字节一致（确定性）。
	if string(png1) != string(png2) {
		t.Fatal("rendering is not deterministic")
	}
	if m1.SHA256 != m2.SHA256 || len(m1.SHA256) != 64 {
		t.Fatalf("manifest hash wrong: %q", m1.SHA256)
	}
	if !m1.Key || m1.AudioN != 2 || m1.File != "frame_0000.png" {
		t.Fatalf("manifest fields wrong: %+v", m1)
	}
	if err := r.WriteFrame(0, png1); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "frame_0000.png"))
	if err != nil || st.Size() == 0 {
		t.Fatalf("frame not written: %v", err)
	}
}

func TestUntrustedFrameMarked(t *testing.T) {
	dir := t.TempDir()
	r, _ := render.New(dir)
	good, mg, _ := r.RenderSlot(slot(1, 27_900_000, false, false, 0), 1)
	bad, mb, _ := r.RenderSlot(slot(2, 28_800_000, false, true, 0), 2)
	if mb.Untrusted != true || mg.Untrusted != false {
		t.Fatalf("untrusted manifest flags: %+v %+v", mg, mb)
	}
	// 不可信帧渲染像素必然不同（状态色变化）。
	if string(good) == string(bad) {
		t.Fatal("untrusted frame must render distinct pixels")
	}
}
