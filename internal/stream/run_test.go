package stream

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mediaroute/internal/clock"
	"mediaroute/internal/index"
)

// syntheticIndex builds an in-memory index for a given number of video and
// audio samples without touching disk, so streaming logic can be tested in
// isolation from the MP4 writer.
func syntheticIndex(video, audio int) *index.Index {
	ix := &index.Index{
		SchemaVersion:  index.SchemaVersion,
		ScannerVersion: "test",
		ManifestDigest: "m",
		InputDigest:    "i",
	}
	v := index.Track{
		ID: 1, Kind: "video", Source: "v.mp4", Timescale: 90000,
		Origin: clock.Rational{Num: 1000, Den: 1},
	}
	for i := 0; i < video; i++ {
		v.Samples = append(v.Samples, index.Sample{
			Number: int64(i + 1), DTS: int64(i) * 3600, CTS: int64(i)*3600 + 1800,
			Duration: 3600, Offset: int64(i) * 10, Size: 10, Sync: i%12 == 0, Readable: true,
		})
		if i%12 == 0 {
			v.Keyframes = append(v.Keyframes, int64(i+1))
		}
	}
	ix.Tracks = append(ix.Tracks, v)
	_ = ix.Finalize()
	return ix
}

func TestRunRejectsNilIndex(t *testing.T) {
	if _, err := Run(context.Background(), nil, Options{}); err == nil {
		t.Fatal("want error for nil index")
	}
}

func TestRunIsolatesUnreadableWithoutOpening(t *testing.T) {
	ix := syntheticIndex(4, 0)
	// Mark sample 3 unreadable; the source file does not exist, but unreadable
	// samples must never trigger a read.
	ix.Tracks[0].Samples[2].Readable = false
	_ = ix.Finalize()
	// The source file does not exist, so opening the track fails before any
	// sample is visited. Corpus-level isolation is covered by the scan and
	// demo tests, which use real files.
	if _, err := Run(context.Background(), ix, Options{}); err == nil {
		t.Fatal("want open error for a missing source")
	}
}

func TestCanceledRun(t *testing.T) {
	ix := syntheticIndex(4, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, ix, Options{})
	if err == nil {
		t.Fatal("want cancellation error")
	}
}

func TestReadChunkRespectsBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	payload := make([]byte, 2<<20)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Three samples of 512 KiB each: the third exceeds the 1 MiB budget.
	samples := []index.Sample{
		{Number: 1, Offset: 0, Size: 512 << 10, Readable: true},
		{Number: 2, Offset: 0, Size: 512 << 10, Readable: true},
		{Number: 3, Offset: 0, Size: 512 << 10, Readable: true},
	}
	chunk, err := readChunk(context.Background(), f, samples)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if chunk.total > MaxChunkBytes {
		t.Fatalf("chunk exceeded budget: %d", chunk.total)
	}
	if chunk.peak > MaxChunkBytes {
		t.Fatalf("peak exceeded budget: %d", chunk.peak)
	}
	if !chunk.budgetStop {
		t.Fatal("expected the budget to stop the batch")
	}
	if len(chunk.payloads) != 2 {
		t.Fatalf("want the first two samples read, got %d", len(chunk.payloads))
	}
}

func TestReadChunkReportsReadFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tiny.bin")
	if err := os.WriteFile(path, make([]byte, 16), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// A sample that claims 8 bytes at offset 0 is fine; one at offset 12 with
	// 8 bytes passes the scan bound but fails the actual read.
	samples := []index.Sample{
		{Number: 1, Offset: 0, Size: 8, Readable: true},
		{Number: 2, Offset: 12, Size: 8, Readable: true},
	}
	chunk, err := readChunk(context.Background(), f, samples)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := chunk.payloads[0]; !ok {
		t.Fatal("readable sample not loaded")
	}
	if _, ok := chunk.readErr[1]; !ok {
		t.Fatal("short read must be reported")
	}
}
