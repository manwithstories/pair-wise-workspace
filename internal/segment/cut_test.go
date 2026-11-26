package segment

import (
	"testing"

	"audiorestore/internal/model"
	"audiorestore/internal/wav"
)

func makeBlocks(n int, bad map[int]string) []wav.FrameBlock {
	blocks := make([]wav.FrameBlock, n)
	for i := range blocks {
		blocks[i] = wav.FrameBlock{
			Index: i, StartFrame: int64(i * 10), Frames: 10,
			Data: make([]byte, 40),
		}
		switch bad[i] {
		case "missing":
			blocks[i].Missing = true
			blocks[i].Data = nil
		case "corrupt":
			blocks[i].Corrupt = true
			blocks[i].Data = nil
		case "parity":
			blocks[i].Parity = true
			blocks[i].Data = nil
		}
	}
	return blocks
}

func TestCutBoundariesOnWholeFrames(t *testing.T) {
	blocks := makeBlocks(6, nil)
	segs, err := Cut("t", 48000, blocks, Config{WindowFrames: 15, KeepPartial: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range segs {
		if (s.EndFrame-s.StartFrame)%1 != 0 {
			t.Fatalf("segment %s not frame aligned", s.ID)
		}
		if s.StartPTS.Cmp(model.SampleToPTS(s.StartFrame, 48000)) != 0 {
			t.Fatalf("segment PTS mismatch at %s", s.ID)
		}
	}
	// 60 frames / 15-frame windows = 4 segments.
	if len(segs) != 4 {
		t.Fatalf("expected 4 segments, got %d", len(segs))
	}
}

func TestGapSplitsSegmentsWithoutSpanning(t *testing.T) {
	blocks := makeBlocks(5, map[int]string{2: "missing"})
	segs, err := Cut("t", 48000, blocks, Config{WindowFrames: 10, KeepPartial: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range segs {
		for _, idx := range s.SourceBlocks {
			if idx == 2 {
				t.Fatalf("segment %s spans the missing block", s.ID)
			}
		}
		if len(s.SourceBlocks) == 0 {
			t.Fatalf("segment %s has no provenance", s.ID)
		}
	}
	// Two contiguous runs (blocks 0-1 and 3-4), each cut into two
	// 10-frame windows, so the gap yields four segments total.
	if len(segs) != 4 {
		t.Fatalf("expected 4 segments around the gap, got %d", len(segs))
	}
}

func TestParityBlocksExcludedFromSegments(t *testing.T) {
	blocks := makeBlocks(4, map[int]string{3: "parity"})
	segs, err := Cut("t", 48000, blocks, Config{WindowFrames: 10, KeepPartial: true})
	if err != nil {
		t.Fatal(err)
	}
	totalFrames := int64(0)
	for _, s := range segs {
		totalFrames += s.EndFrame - s.StartFrame
	}
	if totalFrames != 30 {
		t.Fatalf("expected 30 audio frames (parity excluded), got %d", totalFrames)
	}
}

func TestInvalidWindowRejected(t *testing.T) {
	if _, err := Cut("t", 48000, makeBlocks(2, nil), Config{WindowFrames: 0}); err == nil {
		t.Fatal("expected error for non-positive window")
	}
}

func TestPartialWindowDroppedWhenConfigured(t *testing.T) {
	blocks := makeBlocks(3, nil) // 30 frames
	segs, err := Cut("t", 48000, blocks, Config{WindowFrames: 20, KeepPartial: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 || segs[0].EndFrame-segs[0].StartFrame != 20 {
		t.Fatalf("expected one full 20-frame segment, got %+v", segs)
	}
}
