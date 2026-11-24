package fixtures

import (
	"os"
	"path/filepath"
	"testing"

	mp4 "github.com/abema/go-mp4"
)

func probeTrack(t *testing.T, path string) mp4.Tracks {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := mp4.Probe(f)
	if err != nil {
		t.Fatalf("probe %s: %v", filepath.Base(path), err)
	}
	return info.Tracks
}

func TestBuildNormal(t *testing.T) {
	dir := t.TempDir()
	spec := DefaultSpec(42)
	a, err := Build(dir, spec, VariantNormal)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if a.VideoBytes == 0 || a.AudioBytes == 0 {
		t.Fatal("empty media files")
	}
	vt := probeTrack(t, a.VideoPath)
	if len(vt) != 1 || vt[0].Timescale != 90000 {
		t.Fatalf("video track unexpected: %+v", vt)
	}
	if len(vt[0].Samples) != spec.VideoSamples {
		t.Fatalf("want %d samples got %d", spec.VideoSamples, len(vt[0].Samples))
	}
	if len(vt[0].EditList) != 2 || vt[0].EditList[0].MediaTime != -1 {
		t.Fatalf("edit list not preserved: %+v", vt[0].EditList)
	}
	if vt[0].AVC == nil || vt[0].AVC.Width != 320 {
		t.Fatalf("avc config missing: %+v", vt[0].AVC)
	}
	at := probeTrack(t, a.AudioPath)
	if at[0].MP4A == nil || at[0].MP4A.ChannelCount != 2 {
		t.Fatalf("audio entry missing: %+v", at[0].MP4A)
	}
	if at[0].Timescale != 48000 {
		t.Fatalf("want 48k audio got %d", at[0].Timescale)
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	a, err := Build(t.TempDir(), DefaultSpec(7), VariantNormal)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(t.TempDir(), DefaultSpec(7), VariantNormal)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := os.ReadFile(a.VideoPath)
	y, _ := os.ReadFile(b.VideoPath)
	if string(x) != string(y) {
		t.Fatal("same seed must produce identical video bytes")
	}
}

func TestBuildTruncated(t *testing.T) {
	a, err := Build(t.TempDir(), DefaultSpec(42), VariantTruncated)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(a.VideoPath)
	if err != nil {
		t.Fatal(err)
	}
	tracks := probeTrack(t, a.VideoPath)
	last := tracks[0].Chunks[len(tracks[0].Chunks)-1].DataOffset
	lastSize := uint64(tracks[0].Samples[len(tracks[0].Samples)-1].Size)
	if last+lastSize <= uint64(st.Size()) {
		t.Fatalf("tail not truncated: last sample ends %d, file %d", last+lastSize, st.Size())
	}
}

func TestBuildOutOfRange(t *testing.T) {
	a, err := Build(t.TempDir(), DefaultSpec(42), VariantOutOfRange)
	if err != nil {
		t.Fatal(err)
	}
	tracks := probeTrack(t, a.VideoPath)
	if tracks[0].Chunks[5].DataOffset != 1<<30 {
		t.Fatalf("chunk 5 offset not patched: %d", tracks[0].Chunks[5].DataOffset)
	}
}

func TestAllVariantsBuild(t *testing.T) {
	dir := t.TempDir()
	for _, v := range AllVariants() {
		if _, err := Build(dir, DefaultSpec(1), v); err != nil {
			t.Fatalf("variant %s: %v", v, err)
		}
	}
}
