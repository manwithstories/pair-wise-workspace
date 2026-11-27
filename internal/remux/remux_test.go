package remux

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"

	"github.com/disintegration/imaging"

	"relay/internal/demux"
	"relay/internal/media"
	"relay/internal/timeline"
)

// pngFrame 用 imaging 生成真实 PNG 载荷，保证验证环节能实际解码。
func pngFrame(t *testing.T, cam uint16, seq uint64, ns media.TimeNS) *media.Frame {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 6))
	img.Set(0, 0, color.NRGBA{R: 255, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, img, imaging.PNG); err != nil {
		t.Fatal(err)
	}
	return &media.Frame{
		Version: media.WireVersion1, Codec: media.CodecPNG,
		CameraID: cam, Sequence: seq, EventNS: ns,
		Width: 8, Height: 6, Payload: buf.Bytes(),
	}
}

func loadManifest(t *testing.T, path string) *Manifest {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func runRoundTrip(t *testing.T, format Format, outName string) {
	dir := t.TempDir()
	out := filepath.Join(dir, outName)
	manPath := filepath.Join(dir, "manifest.json")

	frames := []*media.Frame{
		pngFrame(t, 1, 1, media.Second),
		pngFrame(t, 2, 7, 2*media.Second),
		pngFrame(t, 1, 2, 3*media.Second),
	}
	wantHashes := make([][32]byte, len(frames))
	for i, fr := range frames {
		wantHashes[i] = media.SumSHA256(fr.Payload)
	}

	w, err := NewWriter(out, format)
	if err != nil {
		t.Fatal(err)
	}
	for i, fr := range frames {
		if _, err := w.WriteFrame(&timeline.Envelope{TrackID: uint32(100 + fr.CameraID), Frame: fr}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := w.Close(manPath); err != nil {
		t.Fatal(err)
	}

	// 独立重读，载荷哈希必须与输入完全一致（无损）。
	f, _ := os.Open(out)
	got, err := demux.OpenContainer(f, out)
	f.Close()
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if len(got) != len(frames) {
		t.Fatalf("frame count = %d", len(got))
	}
	for i, fr := range got {
		if media.SumSHA256(fr.Payload) != wantHashes[i] {
			t.Fatalf("frame %d payload bytes changed", i)
		}
	}

	rep, err := VerifyOutput(out, loadManifest(t, manPath))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.Frames != 3 || rep.PayloadOK != 3 || rep.ImageOK != 3 || !rep.ManifestOK {
		t.Fatalf("verify report wrong: %+v", rep)
	}
	if rep.Tracks[101] != 2 || rep.Tracks[102] != 1 {
		t.Fatalf("track tally wrong: %+v", rep.Tracks)
	}
}

func TestRoundTripMFS(t *testing.T) { runRoundTrip(t, FormatMFS, "out.mfs") }
func TestRoundTripMTA(t *testing.T) { runRoundTrip(t, FormatMTA, "out.mta") }

func TestVerifyRejectsCorruptOutput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mfs")
	manPath := filepath.Join(dir, "manifest.json")
	w, _ := NewWriter(out, FormatMFS)
	_, _ = w.WriteFrame(&timeline.Envelope{TrackID: 1, Frame: pngFrame(t, 1, 1, media.Second)})
	_ = w.Close(manPath)

	raw, _ := os.ReadFile(out)
	raw[len(raw)-30] ^= 0x01 // 破坏尾部校验
	_ = os.WriteFile(out, raw, 0o644)
	if _, err := VerifyOutput(out, loadManifest(t, manPath)); err == nil {
		t.Fatal("corrupt output must fail verification")
	}
}

func TestParseFormat(t *testing.T) {
	if f, _ := ParseFormat("out.mta"); f != FormatMTA {
		t.Fatal("mta dispatch")
	}
	if f, _ := ParseFormat("mfs"); f != FormatMFS {
		t.Fatal("mfs dispatch")
	}
	if _, err := ParseFormat("avi"); err == nil {
		t.Fatal("unknown format must error")
	}
}
