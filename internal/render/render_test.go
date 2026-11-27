package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"fieldcut/internal/media"
	"fieldcut/internal/timeline"
)

func solid(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func TestBlendExactWeights(t *testing.T) {
	a := solid(2, 2, color.NRGBA{R: 0, G: 0, B: 0, A: 255})
	b := solid(2, 2, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	for _, tc := range []struct {
		mix  media.Rational
		want uint8
	}{
		{media.NewRational(0, 1), 0},
		{media.NewRational(1, 2), 128},
		{media.NewRational(1, 3), 85},
		{media.NewRational(1, 1), 255},
	} {
		out, err := Blend(a, b, tc.mix)
		if err != nil {
			t.Fatalf("Blend(%s): %v", tc.mix, err)
		}
		if got := out.NRGBAAt(0, 0).R; got != tc.want {
			t.Errorf("mix %s -> %d, want %d", tc.mix, got, tc.want)
		}
	}
	if _, err := Blend(a, solid(3, 3, color.NRGBA{}), media.NewRational(1, 2)); err == nil {
		t.Fatal("size mismatch accepted")
	}
	if _, err := Blend(a, b, media.NewRational(2, 1)); err == nil {
		t.Fatal("mix above one accepted")
	}
}

func TestCompositeMasksAndPriorityOrder(t *testing.T) {
	w, h := 32, 18
	base := solid(w, h, color.NRGBA{R: 0, G: 0, A: 255})
	top := solid(w, h, color.NRGBA{R: 255, G: 0, A: 255})
	out, missing, err := Composite(w, h, []Layer{
		{Track: "bg", Seq: 0, Primary: base},
		{Track: "fg", Seq: 0, Primary: top, Mask: timeline.Rect{X: 8, Y: 4, W: 8, H: 8}},
	})
	if err != nil {
		t.Fatalf("Composite: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	if out.NRGBAAt(10, 6).R != 255 {
		t.Fatalf("masked region not painted: %v", out.NRGBAAt(10, 6))
	}
	if out.NRGBAAt(2, 2).R != 0 {
		t.Fatalf("outside mask painted: %v", out.NRGBAAt(2, 2))
	}
}

func TestCompositeInterpolatedLayer(t *testing.T) {
	w, h := 4, 4
	a := solid(w, h, color.NRGBA{R: 0, A: 255})
	b := solid(w, h, color.NRGBA{R: 255, A: 255})
	out, _, err := Composite(w, h, []Layer{{Track: "a", Seq: 1, Primary: a, Secondary: b, Mix: media.NewRational(1, 3)}})
	if err != nil {
		t.Fatal(err)
	}
	if out.NRGBAAt(0, 0).R != 85 {
		t.Fatalf("interpolated pixel = %d, want 85", out.NRGBAAt(0, 0).R)
	}
}

func TestCompositeReportsMissingAndBadSize(t *testing.T) {
	out, missing, err := Composite(8, 8, []Layer{{Track: "a", Seq: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0].Seq != 3 {
		t.Fatalf("missing = %v", missing)
	}
	if out.Bounds().Dx() != 8 {
		t.Fatal("canvas not created")
	}
	_, _, err = Composite(8, 8, []Layer{{Track: "a", Seq: 0, Primary: solid(4, 4, color.NRGBA{})}})
	var me *media.MediaError
	if !errors.As(err, &me) || me.Kind != media.KindCorrupt {
		t.Fatalf("size mismatch kind = %v", err)
	}
	if _, _, err := Composite(media.MaxWidth+1, 8, nil); err == nil {
		t.Fatal("oversize canvas accepted")
	}
}

func TestCommitIsDeterministicAndAtomic(t *testing.T) {
	dir := t.TempDir()
	ex, _, err := Open(dir, "cfg", "src")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	img := solid(2, 2, color.NRGBA{R: 200, A: 255})
	h1, err := ex.Commit(0, img)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != pngHash(t, img) {
		t.Fatalf("committed hash differs from deterministic PNG")
	}
	if _, err := os.Stat(filepath.Join(dir, "frames", "00000.png.tmp")); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
	if _, ok := ex.Has(0); !ok {
		t.Fatal("hash not recorded for resume")
	}
}

func pngHash(t *testing.T, img image.Image) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

func TestResumeEqualsFullRunAndOwnership(t *testing.T) {
	full := t.TempDir()
	ex, _, err := Open(full, "cfg-1", "src-1")
	if err != nil {
		t.Fatal(err)
	}
	imgs := []*image.NRGBA{solid(2, 2, color.NRGBA{R: 10, A: 255}), solid(2, 2, color.NRGBA{R: 20, A: 255})}
	for i, img := range imgs {
		if _, err := ex.Commit(int64(i), img); err != nil {
			t.Fatal(err)
		}
	}
	if err := ex.Finalize(Ownership{Width: 2, Height: 2, Config: "cfg-1", Source: "src-1"}); err != nil {
		t.Fatal(err)
	}

	// Simulate an interrupted run: frame 0 committed, no export.json, then
	// a fresh Open must adopt it by re-hashing and continue.
	res := t.TempDir()
	crash, _, err := Open(res, "cfg-1", "src-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crash.Commit(0, imgs[0]); err != nil {
		t.Fatal(err)
	}
	ex2, own, err := Open(res, "cfg-1", "src-1")
	if err != nil {
		t.Fatalf("resume open: %v", err)
	}
	if _, ok := ex2.Has(0); !ok || own.Root != "" {
		t.Fatalf("adopted=%v own=%+v", ok, own)
	}
	for i := 1; i < len(imgs); i++ {
		if _, err := ex2.Commit(int64(i), imgs[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := ex2.Finalize(Ownership{Width: 2, Height: 2, Config: "cfg-1", Source: "src-1", Resumed: true}); err != nil {
		t.Fatal(err)
	}
	rootA, _ := ex.RootHash()
	rootB, _ := ex2.RootHash()
	if rootA == "" || rootA != rootB {
		t.Fatalf("resume root %q != full root %q", rootB, rootA)
	}
}

func TestOwnershipRefusesForeignContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Open(dir, "cfg", "src")
	var me *media.MediaError
	if !errors.As(err, &me) || me.Kind != media.KindConflict {
		t.Fatalf("foreign content not refused: %v", err)
	}
}

func TestConfigChangeInvalidatesCache(t *testing.T) {
	dir := t.TempDir()
	ex, _, err := Open(dir, "cfg-a", "src-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Commit(0, solid(2, 2, color.NRGBA{A: 255})); err != nil {
		t.Fatal(err)
	}
	if err := ex.Finalize(Ownership{Config: "cfg-a", Source: "src-a"}); err != nil {
		t.Fatal(err)
	}
	// A changed config invalidates the old cache and restarts cleanly.
	ex2, _, err := Open(dir, "cfg-b", "src-a")
	if err != nil {
		t.Fatalf("config change should invalidate, not fail: %v", err)
	}
	if _, ok := ex2.Has(0); ok {
		t.Fatal("stale frame survived cache invalidation")
	}
	for _, name := range []string{"export.json", "frames/00000.png"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("stale artifact %s survived", name)
		}
	}
	if _, _, err := Open(dir, "cfg-a", "src-a"); err != nil {
		t.Fatalf("same config rejected: %v", err)
	}
}

func TestCheckpointIsAtomicChunked(t *testing.T) {
	dir := t.TempDir()
	ex, _, err := Open(dir, "cfg", "src")
	if err != nil {
		t.Fatal(err)
	}
	img := solid(2, 2, color.NRGBA{A: 255})
	path := filepath.Join(dir, ProgressName)
	if _, err := ex.Commit(0, img); err != nil {
		t.Fatal(err)
	}
	if err := ex.Checkpoint(2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("checkpoint written before chunk boundary")
	}
	if _, err := ex.Commit(1, img); err != nil {
		t.Fatal(err)
	}
	if err := ex.Checkpoint(2); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("checkpoint at chunk boundary: %v", err)
	}
	var p progress
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("torn checkpoint: %v", err)
	}
	if p.Program != "fieldcut" || len(p.Hashes) != 2 || p.Chunk != 2 {
		t.Fatalf("checkpoint = %+v", p)
	}
	// Unchanged commits do not rewrite the checkpoint.
	if _, err := ex.Commit(1, img); err != nil {
		t.Fatal(err)
	}
	if err := ex.Checkpoint(2); err != nil {
		t.Fatal(err)
	}
	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data2) != string(data) {
		t.Fatal("checkpoint rewritten without new frames")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp checkpoint left behind")
	}
}

func TestInvalidationClearsProgress(t *testing.T) {
	dir := t.TempDir()
	ex, _, err := Open(dir, "cfg-a", "src-a")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := ex.Commit(int64(i), solid(2, 2, color.NRGBA{A: 255})); err != nil {
			t.Fatal(err)
		}
	}
	if err := ex.Checkpoint(2); err != nil {
		t.Fatal(err)
	}
	if err := ex.Finalize(Ownership{Config: "cfg-a", Source: "src-a"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(dir, "cfg-b", "src-a"); err != nil {
		t.Fatalf("changed config should invalidate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ProgressName)); !os.IsNotExist(err) {
		t.Fatal("stale checkpoint survived invalidation")
	}
}
