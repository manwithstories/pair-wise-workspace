package metadata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediad/internal/clock"
)

func TestParseMXFDemoMatchesMP4Shape(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteDemoMXF(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := ParseMXF(path)
	if err != nil {
		t.Fatal(err)
	}
	if asset.Container != "mxf" || len(asset.Tracks) != 3 {
		t.Fatalf("asset %+v", asset)
	}
	v0, _ := asset.Track("v0")
	if v0.Rotation != 90 || v0.Timebase != clock.MustNew(1, 30000) {
		t.Fatalf("v0 %+v", v0)
	}
	if v0.Color.Source != SourceMXF || v0.Color.Primaries != PrimariesBT2020 {
		t.Fatalf("v0 color %+v", v0.Color)
	}
	v1, _ := asset.Track("v1")
	if v1.Crop == nil || v1.Crop.W != 1024 || v1.Crop.H != 576 {
		t.Fatalf("v1 crop %+v", v1.Crop)
	}
	a0, _ := asset.Track("a0")
	if a0.SampleRate != 48000 || a0.Channels != 2 || a0.Duration != 96000 {
		t.Fatalf("audio %+v", a0)
	}
}

func TestMXFAndMP4NormalizeIdentically(t *testing.T) {
	dir := t.TempDir()
	mp4Path, err := WriteDemoMP4(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	mxfPath, err := WriteDemoMXF(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	mp4Asset, err := ParseMP4(mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	mxfAsset, err := ParseMXF(mxfPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"v0", "v1", "a0"} {
		a, _ := mp4Asset.Track(key)
		b, _ := mxfAsset.Track(key)
		if a.Timebase != b.Timebase || a.Duration != b.Duration || a.Rotation != b.Rotation {
			t.Fatalf("%s timing differs: mp4=%+v mxf=%+v", key, a, b)
		}
		if a.OutputWidth() != b.OutputWidth() || a.OutputHeight() != b.OutputHeight() {
			t.Fatalf("%s geometry differs: %dx%d vs %dx%d", key,
				a.OutputWidth(), a.OutputHeight(), b.OutputWidth(), b.OutputHeight())
		}
	}
}

func TestParseMXFCorrupt(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteDemoMXF(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := ParseMXF(path)
	if err == nil {
		t.Fatal("expected error for corrupt MXF")
	}
	if len(asset.Failures) == 0 || !strings.Contains(err.Error(), "track=v0") {
		t.Fatalf("failures not track-scoped: %v %+v", err, asset.Failures)
	}
	if _, ok := asset.Track("a0"); !ok {
		t.Fatal("audio branch lost despite isolated corruption")
	}
}

func TestParseMXFRejectsGarbage(t *testing.T) {
	if _, err := ParseMXFBytes("garbage", []byte("not-an-mxf-file")); err == nil {
		t.Fatal("expected magic error")
	}
	if _, err := ParseMXF(filepath.Join(t.TempDir(), "missing.mxf")); err == nil {
		t.Fatal("expected read error")
	}
}

func TestWriteMXFIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mxf")
	b := filepath.Join(dir, "b.mxf")
	if err := WriteMXF(a, demoMXFSpec(false)); err != nil {
		t.Fatal(err)
	}
	if err := WriteMXF(b, demoMXFSpec(false)); err != nil {
		t.Fatal(err)
	}
	ba, _ := os.ReadFile(a)
	bb, _ := os.ReadFile(b)
	if string(ba) != string(bb) {
		t.Fatal("identical specs produced different bytes")
	}
}
