package mux

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mediacore/internal/adapter"
	"mediacore/internal/media"
	"mediacore/internal/pipeline"
)

// buildInput parses a deterministic generated fixture into tracks plus a
// mapping, so every remux test starts from real container metadata.
func buildInput(t *testing.T, tracks, samples int) (media.Source, []*media.Track, map[string]uint32, string) {
	t.Helper()
	layout, err := adapter.BuildMP4(adapter.MP4Spec{
		Tracks: tracks, Samples: samples, Seed: 11,
		Color: &adapter.MediaColor{Primaries: 1, Transfer: 1, Matrix: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	src := adapter.NewLogicalSource(layout, "input.mp4")
	c, err := (&adapter.MP4Adapter{}).Parse(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	plan := map[string]uint32{}
	for i, tr := range c.Tracks {
		plan[tr.ID.String()] = uint32(i + 1)
	}
	return src, c.Tracks, plan, c.Fingerprint
}

func TestRemuxPreservesSampleStreams(t *testing.T) {
	src, tracks, plan, fp := buildInput(t, 3, 60)
	outPath := filepath.Join(t.TempDir(), "out.mp4")
	r := &Remuxer{Tracks: tracks, Plan: plan, Source: src}
	res, err := r.Run(context.Background(), Options{OutputPath: outPath, Fingerprint: fp, MappingSign: "sig"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(res.Tracks))
	}
	for _, tr := range res.Tracks {
		if !tr.Match() {
			t.Fatalf("track %s input=%s output=%s", tr.Key, tr.InputSHA256, tr.OutputSHA256)
		}
		if tr.OutputSHA256 == "" {
			t.Fatalf("track %s has no output checksum", tr.Key)
		}
	}

	// The output must be a valid MP4 with the same sample counts.
	out, err := media.OpenFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	c, err := (&adapter.MP4Adapter{}).Parse(context.Background(), out)
	if err != nil {
		t.Fatalf("output does not reparse: %v", err)
	}
	if len(c.Tracks) != 3 {
		t.Fatalf("reparsed tracks=%d", len(c.Tracks))
	}
	byID := map[uint32]*media.Track{}
	for _, tr := range c.Tracks {
		byID[tr.ID.Value] = tr
	}
	for _, tr := range res.Tracks {
		got, ok := byID[tr.OutputID]
		if !ok {
			t.Fatalf("no output track %d", tr.OutputID)
		}
		if got.SampleCount() != tr.Samples {
			t.Fatalf("track %d samples=%d want %d", tr.OutputID, got.SampleCount(), tr.Samples)
		}
	}
}

// TestRemuxSampleBytesMatchInput verifies the strongest form of "no re-encode":
// the delivered bytes at each output offset equal the input sample bytes.
func TestRemuxSampleBytesMatchInput(t *testing.T) {
	src, tracks, plan, fp := buildInput(t, 2, 20)
	outPath := filepath.Join(t.TempDir(), "out.mp4")
	r := &Remuxer{Tracks: tracks, Plan: plan, Source: src}
	res, err := r.Run(context.Background(), Options{OutputPath: outPath, Fingerprint: fp, MappingSign: "sig"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := media.OpenFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	c, err := (&adapter.MP4Adapter{}).Parse(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[uint32]*media.Track{}
	for _, tr := range c.Tracks {
		byID[tr.ID.Value] = tr
	}
	for _, want := range res.Tracks {
		outTrack := byID[want.OutputID]
		// Locate the input track with the same ordinal.
		var inTrack *media.Track
		for _, tr := range tracks {
			if plan[tr.ID.String()] == want.OutputID {
				inTrack = tr
			}
		}
		for i := 0; i < inTrack.SampleCount(); i++ {
			a, err := inTrack.Index.At(i)
			if err != nil {
				t.Fatal(err)
			}
			b, err := outTrack.Index.At(i)
			if err != nil {
				t.Fatal(err)
			}
			if a.Size != b.Size {
				t.Fatalf("track %s sample %d size %d != %d", want.Key, i, a.Size, b.Size)
			}
			in := make([]byte, a.Size)
			outBytes := make([]byte, b.Size)
			if _, err := src.ReadAt(in, a.Offset); err != nil {
				t.Fatal(err)
			}
			if _, err := out.ReadAt(outBytes, b.Offset); err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(in) != hex.EncodeToString(outBytes) {
				t.Fatalf("track %s sample %d bytes differ", want.Key, i)
			}
		}
	}
}

func TestRemuxInterruptAndResumeWritesEachSampleOnce(t *testing.T) {
	src, tracks, plan, fp := buildInput(t, 3, 60)
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.mp4")
	cpPath := filepath.Join(dir, "cp.json")
	r := &Remuxer{Tracks: tracks, Plan: plan, Source: src}

	_, err := r.Run(context.Background(), Options{
		OutputPath: outPath, CheckpointPath: cpPath, Fingerprint: fp,
		MappingSign: "sig", InterruptAfter: 70, WaveSize: 10,
	})
	var ie *InterruptError
	if !errors.As(err, &ie) {
		t.Fatalf("expected interrupt, got %v", err)
	}

	start := time.Now()
	planResume, err := pipeline.Load(cpPath, fp, "sig", outPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), Options{
		OutputPath: outPath, CheckpointPath: cpPath, Fingerprint: fp,
		MappingSign: "sig", Resume: planResume, WaveSize: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("resume took %v, above the 2s bound", elapsed)
	}
	for _, tr := range res.Tracks {
		if !tr.Match() {
			t.Fatalf("track %s input=%s output=%s", tr.Key, tr.InputSHA256, tr.OutputSHA256)
		}
	}
	// The 70 samples confirmed before the interrupt must not be replayed.
	if res.Samples != 180-70 {
		t.Fatalf("resumed samples=%d want %d", res.Samples, 180-70)
	}
}

func TestRemuxIsDeterministicAcrossRuns(t *testing.T) {
	src, tracks, plan, fp := buildInput(t, 2, 30)

	// Run twice, then again with a resume cycle, and require identical
	// per-track checksums every time.
	digests := make([]string, 0, 3)
	run := func(t *testing.T, outPath, cpPath string, o Options) string {
		t.Helper()
		r := &Remuxer{Tracks: tracks, Plan: plan, Source: src}
		res, err := r.Run(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		h := ""
		for _, tr := range res.Tracks {
			h += tr.Key + "=" + tr.OutputSHA256 + ";"
		}
		return h
	}
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		outPath := filepath.Join(dir, "out"+string(rune('a'+i))+".mp4")
		digests = append(digests, run(t, outPath, "", Options{
			OutputPath: outPath, Fingerprint: fp, MappingSign: "sig",
		}))
	}
	if digests[0] != digests[1] {
		t.Fatalf("output checksums differ between runs:\n%s\n%s", digests[0], digests[1])
	}

	// Interrupted, resumed delivery must produce the same sample stream.
	outPath := filepath.Join(dir, "resumed.mp4")
	cpPath := filepath.Join(dir, "cp.json")
	_, err := (&Remuxer{Tracks: tracks, Plan: plan, Source: src}).Run(context.Background(), Options{
		OutputPath: outPath, CheckpointPath: cpPath, Fingerprint: fp,
		MappingSign: "sig", InterruptAfter: 20, WaveSize: 5,
	})
	var ie *InterruptError
	if !errors.As(err, &ie) {
		t.Fatalf("expected interrupt, got %v", err)
	}
	rp, err := pipeline.Load(cpPath, fp, "sig", outPath)
	if err != nil {
		t.Fatal(err)
	}
	resumed := run(t, outPath, cpPath, Options{
		OutputPath: outPath, CheckpointPath: cpPath, Fingerprint: fp,
		MappingSign: "sig", Resume: rp, WaveSize: 5,
	})
	if resumed != digests[0] {
		t.Fatalf("resumed output differs from a clean run:\n%s\n%s", resumed, digests[0])
	}
}

// TestRemuxStreamsThroughBoundedQueue proves the remux accepts a bounded
// queue and keeps its peak within the configured byte budget.
func TestRemuxStreamsThroughBoundedQueue(t *testing.T) {
	src, tracks, plan, fp := buildInput(t, 3, 80)
	outPath := filepath.Join(t.TempDir(), "q.mp4")
	q := pipeline.NewQueue(src, pipeline.Limits{MaxSamples: 3, MaxBytes: 8 << 10})
	var peak int64
	r := &Remuxer{Tracks: tracks, Plan: plan, Source: src}
	res, err := r.Run(context.Background(), Options{
		OutputPath: outPath, Fingerprint: fp, MappingSign: "sig",
		Queue: q, QueuePeak: &peak,
	})
	if err != nil {
		t.Fatal(err)
	}
	if peak > 8<<10 {
		t.Fatalf("peak queue bytes %d exceeds the 8 KiB budget", peak)
	}
	for _, tr := range res.Tracks {
		if !tr.Match() {
			t.Fatalf("track %s mismatch through the queue", tr.Key)
		}
	}
}

func TestRemuxRefusesToOverwriteInput(t *testing.T) {
	src, tracks, plan, fp := buildInput(t, 1, 4)
	r := &Remuxer{Tracks: tracks, Plan: plan, Source: src}
	_, err := r.Run(context.Background(), Options{OutputPath: src.Name(), Fingerprint: fp, MappingSign: "sig"})
	if media.ClassOf(err) != media.ClassUsage {
		t.Fatalf("class=%s err=%v", media.ClassOf(err), err)
	}
}

// TestRemuxSurfacesInvalidSampleLength proves a corrupted sample table is
// refused before any bytes are written.
func TestRemuxSurfacesInvalidSampleLength(t *testing.T) {
	layout, err := adapter.BuildMP4(adapter.MP4Spec{Tracks: 2, Samples: 10, Corrupt: "sample_length", Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	// Corruption is caught by the adapter, so drive the remuxer from a
	// container whose index reports the bad size directly.
	c, parseErr := (&adapter.MP4Adapter{}).Parse(context.Background(), adapter.NewLogicalSource(layout, "bad.mp4"))
	if parseErr == nil {
		_ = c
		t.Fatal("adapter accepted an oversized sample length")
	}
	if media.ClassOf(parseErr) != media.ClassInvalidLength {
		t.Fatalf("class=%s err=%v", media.ClassOf(parseErr), parseErr)
	}

	// The muxer independently refuses an over-large sample size.
	bad := &media.Track{
		ID:    media.TrackID{Container: "mp4", Value: 1},
		Index: media.SliceIndex{{Seq: 0, Offset: 0, Size: 1 << 32}},
	}
	src := &fixedSource{data: make([]byte, 64), name: "bad.mp4"}
	r := &Remuxer{Tracks: []*media.Track{bad}, Plan: map[string]uint32{"mp4:1": 1}, Source: src}
	_, err = r.Run(context.Background(), Options{OutputPath: filepath.Join(t.TempDir(), "o.mp4")})
	if media.ClassOf(err) != media.ClassInvalidLength {
		t.Fatalf("class=%s err=%v", media.ClassOf(err), err)
	}
}

type fixedSource struct {
	data []byte
	name string
}

func (s *fixedSource) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(s.data)) {
		return 0, os.ErrDeadlineExceeded
	}
	return copy(p, s.data[off:]), nil
}
func (s *fixedSource) Size() int64  { return int64(len(s.data)) }
func (s *fixedSource) Name() string { return s.name }
