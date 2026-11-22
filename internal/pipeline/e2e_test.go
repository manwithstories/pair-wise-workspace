package pipeline_test

import (
	"context"
	"testing"

	"mediapack/internal/codec"
	"mediapack/internal/fault"
	"mediapack/internal/fixture"
	"mediapack/internal/index"
	"mediapack/internal/pipeline"
)

// Fault class 1: a truncated NAL travels from the parser through the whole
// pipeline and must surface as a KindCorruptInput fault with a byte offset.
func TestE2ETruncatedNALThroughPipeline(t *testing.T) {
	data, _, keyOffs, _ := fixture.BuildStream(fixture.StreamSpec{
		Codec: codec.H265, Seed: 21, Frames: 24, KeyEvery: 6,
	}, fixture.Fault{})
	cut := keyOffs[2] + 14
	bad := data[:cut]
	p, _ := pipeline.New(pipeline.Config{
		SourceID: "corrupt", ChunkBytes: 2048, WorkDir: t.TempDir(),
	})
	_, err := p.Run(context.Background(), bad)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindCorruptInput {
		t.Fatalf("expected corrupt input fault, got %v", err)
	}
	if fe.Offset < 0 || fe.Offset > cut {
		t.Fatalf("damage offset %d outside input [0,%d]", fe.Offset, cut)
	}
}

// Fault class 2: an out-of-range keyframe index is rejected independently of
// the healthy bitstream, proving the index path has its own fault boundary.
func TestE2EBadIndexIndependentOfStream(t *testing.T) {
	data, _, _, _ := fixture.BuildStream(fixture.StreamSpec{
		Codec: codec.AV1, Seed: 21, Frames: 16, KeyEvery: 4,
	}, fixture.Fault{})
	badIndex := fixture.BuildBadIndex(fixture.BadIndexOutOfRange, int64(len(data)))
	_, _, err := index.Parse(badIndex, int64(len(data)))
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindCorruptInput {
		t.Fatalf("expected index corruption, got %v", err)
	}
	// The same stream is otherwise perfectly processable.
	p, _ := pipeline.New(pipeline.Config{
		SourceID: "healthy", ChunkBytes: 2048, WorkDir: t.TempDir(),
	})
	if _, err := p.Run(context.Background(), data); err != nil {
		t.Fatalf("healthy stream must still run: %v", err)
	}
}

func TestE2EInputSizeLimit(t *testing.T) {
	oversized := make([]byte, pipeline.MaxInputBytes+1)
	p, _ := pipeline.New(pipeline.Config{
		SourceID: "big", ChunkBytes: 2048, WorkDir: t.TempDir(),
	})
	_, err := p.Run(context.Background(), oversized)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindLimit {
		t.Fatalf("expected limit fault, got %v", err)
	}
}

// Fault class 3: interruption followed by resume exercises recovery, and the
// recovered run must not rewrite already-durable fragments.
func TestE2ERecoverySkipsExistingFragments(t *testing.T) {
	data, _, _, _ := fixture.BuildStream(fixture.StreamSpec{
		Codec: codec.H264, Seed: 33, Frames: 40, KeyEvery: 8, ExtraSize: 64,
	}, fixture.Fault{})
	dir := t.TempDir()
	p1, _ := pipeline.New(pipeline.Config{
		SourceID: "rec", ChunkBytes: 1024, InterruptAt: 5, WorkDir: dir,
	})
	if _, err := p1.Run(context.Background(), data); err == nil {
		t.Fatal("expected interruption")
	}
	p2, _ := pipeline.New(pipeline.Config{
		SourceID: "rec", ChunkBytes: 1024, WorkDir: dir,
	})
	res, err := p2.Resume(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Metrics.CacheRevalidated || res.Metrics.Fragments < 5 {
		t.Fatalf("recovery metrics wrong: %+v", res.Metrics)
	}
}
