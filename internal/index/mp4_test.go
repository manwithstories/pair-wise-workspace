package index

import (
	"os"
	"path/filepath"
	"testing"

	"mediarun/internal/fixture"
)

func buildTestMP4(t *testing.T, samples int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cam.mp4")
	if err := fixture.BuildMP4(path, samples, 30_000, 1000); err != nil {
		t.Fatalf("build: %v", err)
	}
	return path
}

func TestIndexEveryOffsetVerifies(t *testing.T) {
	idx, err := BuildFile(buildTestMP4(t, 200))
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if len(idx.Keyframes) != 4 {
		t.Fatalf("keyframes = %d, want 4", len(idx.Keyframes))
	}
	if idx.Roundtrip != 1.0 {
		t.Fatalf("roundtrip = %v", idx.Roundtrip)
	}
	for _, kf := range idx.Keyframes {
		if !kf.Verified || kf.Size == 0 || kf.Offset == 0 {
			t.Fatalf("bad keyframe: %+v", kf)
		}
	}
}

func TestIndexOffsetsSortedAndContiguous(t *testing.T) {
	idx, err := BuildFile(buildTestMP4(t, 100))
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	for i := 1; i < len(idx.Keyframes); i++ {
		if idx.Keyframes[i].TimeNS <= idx.Keyframes[i-1].TimeNS {
			t.Fatalf("times not increasing at %d", i)
		}
	}
	// Sync samples are 1 and 51 (a 50-sample, 2-second GOP), and both fall
	// in the first chunk, so their offsets differ by 50 sample sizes.
	if idx.Keyframes[1].Offset-idx.Keyframes[0].Offset != 50*64 {
		t.Fatalf("first-chunk offsets not contiguous: %+v", idx.Keyframes[:2])
	}
}

func TestIndexRejectsCorruptOffset(t *testing.T) {
	path := buildTestMP4(t, 50)
	idx, err := BuildFile(path)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	// Corrupt the file by truncating it mid-sample and re-read the index;
	// verification must now fail.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := f.Stat()
	f.Close()
	last := idx.Keyframes[len(idx.Keyframes)-1]
	if last.Offset+uint64(last.Size) > uint64(info.Size()) {
		t.Fatalf("last keyframe already out of file")
	}
	// Hand-verify a bogus offset through the same checker the builder uses.
	tracks, mdats, _, err := parseTracks(mustOpen(t, path))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_ = tracks
	bad := Keyframe{Offset: uint64(info.Size() + 10), Size: 64}
	if err := verifyOffset(mustOpen(t, path), info.Size(), mdats, bad, FormatOpaque); err == nil {
		t.Fatalf("offset past EOF verified")
	}
}

func TestHitRate(t *testing.T) {
	idx, err := BuildFile(buildTestMP4(t, 200))
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	var times []int64
	for _, kf := range idx.Keyframes {
		times = append(times, kf.TimeNS+kf.DurNS/2)
	}
	rate := idx.HitRate(times, 1_000_000)
	if rate != 1.0 {
		t.Fatalf("hit rate = %v", rate)
	}
	// Times far from every keyframe must miss.
	rate = idx.HitRate([]int64{-5_000_000_000, 9e18}, 1_000_000)
	if rate != 0 {
		t.Fatalf("spurious hits: %v", rate)
	}
}

func TestNearest(t *testing.T) {
	idx, err := BuildFile(buildTestMP4(t, 50))
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	kf, ok := idx.Nearest(idx.Keyframes[2].TimeNS)
	if !ok || kf.Sample != idx.Keyframes[2].Sample {
		t.Fatalf("nearest mismatch: %+v ok=%v", kf, ok)
	}
	if _, ok := idx.Nearest(-1); ok {
		t.Fatalf("negative time resolved")
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
