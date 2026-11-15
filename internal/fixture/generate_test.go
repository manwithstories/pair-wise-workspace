package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"mp4probe/internal/index"
	"mp4probe/internal/moov"
)

func smallParams() Params { return Params{VideoSamples: 200, AudioSamples: 160} }

func genOne(t *testing.T, dir, name string) (string, *index.Index) {
	t.Helper()
	paths, err := Generate(dir, []string{name}, 42, smallParams())
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	m, err := moov.Parse(f, uint64(st.Size()))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	idx, err := index.Build(filepath.Base(paths[0]), uint64(st.Size()), m)
	if err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	if err := idx.Validate(); err != nil {
		t.Fatalf("validate %s: %v", name, err)
	}
	return paths[0], idx
}

func TestNormalFixture(t *testing.T) {
	dir := t.TempDir()
	_, idx := genOne(t, dir, "normal")
	if len(idx.Tracks) != 2 {
		t.Fatalf("tracks=%d", len(idx.Tracks))
	}
	v := idx.Tracks[0]
	if v.Codec != "avc1" || v.SampleCount != 200 || v.Timescale != 90000 {
		t.Fatalf("video track: %+v", v)
	}
	if v.SyncSampleCount != 20 {
		t.Fatalf("sync count=%d", v.SyncSampleCount)
	}
	a := idx.Tracks[1]
	if !a.AllSync || a.Codec != "mp4a" {
		t.Fatalf("audio track: %+v", a)
	}
	// stz2 sizes must be the small audio sizes.
	for _, s := range a.Sizes {
		if s < 12 || s > 19 {
			t.Fatalf("stz2 size %d out of expected range", s)
		}
	}
	if idx.UnknownBoxes != 1 {
		t.Fatalf("unknown boxes=%d, want 1", idx.UnknownBoxes)
	}
}

func TestEditlistFixture(t *testing.T) {
	dir := t.TempDir()
	_, idx := genOne(t, dir, "editlist")
	v := idx.Tracks[0]
	if v.ElstSegments != 3 || !v.ElstConflict {
		t.Fatalf("elst: segments=%d conflict=%v", v.ElstSegments, v.ElstConflict)
	}
	if v.Elst[0].MediaTime != -1 {
		t.Fatalf("first segment should be empty edit: %+v", v.Elst[0])
	}
	if v.Elst[2].Rate != 1<<15 {
		t.Fatalf("third segment rate=%x", v.Elst[2].Rate)
	}
}

func TestCo64Fixture(t *testing.T) {
	dir := t.TempDir()
	_, idx := genOne(t, dir, "co64")
	if idx.Tracks[0].SampleCount != 200 {
		t.Fatalf("sample count=%d", idx.Tracks[0].SampleCount)
	}
}

func TestMoovtailFixture(t *testing.T) {
	dir := t.TempDir()
	path, idx := genOne(t, dir, "moovtail")
	if idx.MoovOffset < idx.MdatOffset {
		t.Fatalf("moov should be at tail: moov=%d mdat=%d", idx.MoovOffset, idx.MdatOffset)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Size != uint64(st.Size()) {
		t.Fatalf("size mismatch")
	}
}

func TestWrapEvents(t *testing.T) {
	dir := t.TempDir()
	// Custom spec: huge delta forces the 64-bit accumulation across
	// 2^32 exactly once with only 2000 samples.
	v := &trackSpec{
		id: 1, handler: "vide", codec: "avc1",
		timescale: 90000, delta: 3600 * 1000, count: 2000,
		sizes:           makeSizes(7, 2000, 24, 16),
		syncEvery:       20,
		samplesPerChunk: 20,
		tkhdDuration:    48000,
	}
	spec := &fileSpec{
		movieTimescale: 90000,
		movieDuration:  48000,
		tracks:         []*trackSpec{v},
	}
	path := filepath.Join(dir, "wrapsmall.mp4")
	if err := writeFile(path, spec, 7); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	m, err := moov.Parse(f, uint64(st.Size()))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.Build("wrapsmall.mp4", uint64(st.Size()), m)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Tracks[0].WrapEvents != 1 {
		t.Fatalf("wrap events=%d, want 1", idx.Tracks[0].WrapEvents)
	}
	// DTS must be monotonic across the wrap.
	dts := idx.Tracks[0].DTS
	for i := 1; i < len(dts); i++ {
		if dts[i] <= dts[i-1] {
			t.Fatalf("dts regression at %d", i)
		}
	}
}
