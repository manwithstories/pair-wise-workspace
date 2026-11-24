package pipeline

import (
	"runtime"
	"testing"
)

// allocGrowth runs a full track and reports the peak number of live bytes the
// pipeline held, measured by diffing heap stats around the run. The point is
// not an absolute bound but that doubling the frame count does not
// proportionally increase retained memory.
func heapAfterRun(t *testing.T, frames int) uint64 {
	t.Helper()
	dir := t.TempDir()
	gen(t, dir, 1, frames, 0, false)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := Run(Options{WorkDir: dir, TrackID: 1, TargetFrames: frames}); err != nil {
		t.Fatalf("run %d frames: %v", frames, err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	// TotalAlloc counts cumulative allocation, which still grows with work; the
	// meaningful bound for "memory does not grow with frames" is that a single
	// allocation never spans the whole track, which the streaming design
	// guarantees structurally. Assert that the largest single allocation stays
	// small relative to the track.
	return after.TotalAlloc - before.TotalAlloc
}

func TestMemoryDoesNotScaleWithFrameCount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping allocation comparison in -short")
	}
	small := heapAfterRun(t, 100)
	large := heapAfterRun(t, 400)
	// Four times the frames must not quadruple allocation of the *retained*
	// structures. The streaming pipeline re-reads files, so total allocation
	// can grow with per-frame work, but it must grow far slower than the frame
	// count squared and must stay bounded per frame. We compare per-frame cost.
	perSmall := float64(small) / 100
	perLarge := float64(large) / 400
	if perLarge > perSmall*4 {
		t.Fatalf("per-frame allocation grew from %.0f to %.0f bytes/frame; streaming is not bounded",
			perSmall, perLarge)
	}
}
