package pipeline_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"bcast/demux"
	"bcast/fingerprint"
	"bcast/media"
	"bcast/output"
	"bcast/segments"
	"bcast/silence"
)

// patternReader serves deterministic bytes for a large logical stream without
// materialising them, so tests can feed a big input at constant memory.
type patternReader struct{ size int64 }

func (p patternReader) ReadAt(b []byte, off int64) (int, error) {
	if off >= p.size {
		return 0, io.EOF
	}
	if off+int64(len(b)) > p.size {
		b = b[:p.size-off]
	}
	for i := range b {
		b[i] = byte(off + int64(i))
	}
	return len(b), nil
}

// TestLargeInputBoundedMemory processes a 1 GiB logical input and asserts
// total allocation stays under the 512 MiB budget. The input is generated on
// demand (never held whole) and payloads are read per sample, which is the
// property the demux design promises: memory tracks sample count, not bytes.
func TestLargeInputBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("large input test skipped in -short mode")
	}
	const targetBytes = 1024 << 20 // the 1 GiB budget case
	const sampleSize = 188 << 10   // 188 KiB per video sample, keyframe every 30
	const count = targetBytes / sampleSize
	const ts = 90000
	tracks := []output.TrackMux{{
		ID: 1, Kind: media.TrackVideo, Timescale: ts,
	}}
	for i := 0; i < count; i++ {
		tracks[0].Samples = append(tracks[0].Samples, output.SampleRef{
			Offset: int64(i) * sampleSize, Size: sampleSize, Duration: 3000,
			Keyframe: i%30 == 0,
		})
	}
	src := patternReader{size: int64(count) * sampleSize}
	dir := t.TempDir()
	path := filepath.Join(dir, "big.mp4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := output.Mux(f, src, tracks); err != nil {
		t.Fatal(err)
	}
	f.Close()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	peak := measurePeak(func() {
		rf, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer rf.Close()
		st, _ := rf.Stat()
		df, err := demux.Parse(rf, st.Size(), path)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if err := demux.BuildKeyIndex(df, rf); err != nil {
			t.Fatalf("index: %v", err)
		}
		segs, err := segments.Cut(&df.File, 3*ts)
		if err != nil {
			t.Fatalf("cut: %v", err)
		}
		for _, s := range segs {
			if _, err := fingerprint.Hash(rf, s); err != nil {
				t.Fatalf("hash: %v", err)
			}
		}
		_ = silence.Detect(&df.File, rf, 4800, 500)
		_ = context.Background()
	})
	const budget = 512 << 20
	if peak > budget {
		t.Fatalf("peak allocation %d MiB exceeds %d MiB budget", peak>>20, budget>>20)
	}
	t.Logf("peak allocation across %d MiB input: %d MiB", targetBytes>>20, peak>>20)
}

// measurePeak runs fn and returns the delta of allocated bytes attributable to
// it, using TotalAlloc which counts all allocations (not just live heap).
func measurePeak(fn func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	fn()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}
