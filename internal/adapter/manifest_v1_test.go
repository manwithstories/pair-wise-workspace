package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	headerLine = `{"type":"header","version":1,"timebase":{"num":1,"den":90000},` +
		`"tracks":[{"id":"camA","priority":1,"fps":25,"modulus":0}]}`
	anchorLine = `{"type":"anchor","track":"camA","pts":0,"ref_ms":1000}`
)

var frameLine = `{"type":"frame","track":"camA","seq":0,"pts":0,"dur":3600,` +
	`"sha256":"` + strings.Repeat("a", 64) + `","file":"camA/0000.jpg"}`

func TestParseManifestV1(t *testing.T) {
	m, err := ParseManifest(writeManifest(t, headerLine, frameLine, anchorLine), 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != 1 || len(m.Tracks) != 1 || len(m.Frames) != 1 || len(m.Anchors) != 1 {
		t.Fatalf("bad manifest: %+v", m)
	}
	if m.Timebase.Den != 90000 || m.Tracks[0].FPS != 25 {
		t.Fatalf("bad header parse: %+v", m)
	}
}

func TestParseManifestUnknownVersion(t *testing.T) {
	bad := strings.Replace(headerLine, `"version":1`, `"version":99`, 1)
	_, err := ParseManifest(writeManifest(t, bad, frameLine), 0)
	if err == nil || !strings.Contains(err.Error(), ":1:") || !strings.Contains(err.Error(), "99") {
		t.Fatalf("want locatable version error, got %v", err)
	}
}

func TestParseManifestRejects(t *testing.T) {
	cases := map[string]string{
		"traversal": strings.Replace(frameLine, "camA/0000.jpg", "../evil.jpg", 1),
		"badhash":   strings.Replace(frameLine, strings.Repeat("a", 64), "xyz", 1),
		"unktrack":  strings.Replace(frameLine, `"track":"camA"`, `"track":"camB"`, 1),
		"unktype":   `{"type":"mystery"}`,
	}
	for name, line := range cases {
		if _, err := ParseManifest(writeManifest(t, headerLine, line), 0); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// 首行不是 header
	if _, err := ParseManifest(writeManifest(t, frameLine), 0); err == nil {
		t.Fatal("notheader: accepted")
	}
}

func TestSecureJoin(t *testing.T) {
	root := t.TempDir()
	if _, err := SecureJoin(root, "a/b.jpg"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../x", "a/../../x", "/abs/x", ""} {
		if _, err := SecureJoin(root, bad); err == nil {
			t.Fatalf("traversal %q accepted", bad)
		}
	}
}

func TestLoadImageAndVerify(t *testing.T) {
	root := t.TempDir()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.NRGBA{uint8(x * 30), uint8(y * 30), 128, 255})
		}
	}
	jpgPath := filepath.Join(root, "f.jpg")
	w, _ := os.Create(jpgPath)
	if err := jpeg.Encode(w, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	w.Close()
	data, _ := os.ReadFile(jpgPath)
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])

	got, sha, err := LoadImage(root, "f.jpg", 0)
	if err != nil {
		t.Fatal(err)
	}
	if sha != want || got.Bounds().Dx() != 8 {
		t.Fatalf("LoadImage sha=%s bounds=%v", sha, got.Bounds())
	}
	ok, err := VerifySHA256(root, "f.jpg", want, 0)
	if err != nil || !ok {
		t.Fatalf("VerifySHA256 = %v, %v", ok, err)
	}
	ok, _ = VerifySHA256(root, "f.jpg", strings.Repeat("0", 64), 0)
	if ok {
		t.Fatal("hash mismatch accepted")
	}
	if _, _, err := LoadImage(root, "f.jpg", 4); err == nil {
		t.Fatal("oversize image accepted")
	}
	if _, _, err := LoadImage(root, "../f.jpg", 0); err == nil {
		t.Fatal("traversal accepted")
	}
}
