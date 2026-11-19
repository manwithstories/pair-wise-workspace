package render

import (
	"os"
	"path/filepath"
	"testing"

	"review/internal/clock"
	"review/internal/timeline"
)

func testBlock() timeline.Block {
	return timeline.Block{Cam: 0, Index: 0, StartFrame: 0, EndFrame: 8,
		Spans: []timeline.CueSpan{{Track: "a.srt", ByteOffset: 42, StartFrame: 0, EndFrame: 4}}}
}

func TestRenderDeterministic(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	m1, err := RenderBlock(d1, testBlock(), clock.Rate2997, true, "seed")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := RenderBlock(d2, testBlock(), clock.Rate2997, true, "seed")
	if err != nil {
		t.Fatal(err)
	}
	if m1.SHA256 != m2.SHA256 {
		t.Fatalf("same input, different hash: %s vs %s", m1.SHA256, m2.SHA256)
	}
	if len(m1.Frames) != 8 {
		t.Fatalf("frames: %d", len(m1.Frames))
	}
	// Trace record: frame 2 is covered by the cue at offset 42, frame 6 is not.
	if m1.Frames[2].SourceOffset != 42 || m1.Frames[6].SourceOffset != -1 {
		t.Fatalf("bad source mapping: %+v", m1.Frames)
	}
	if m1.Frames[0].MediaTime != "00:00:00;00" {
		t.Fatalf("bad media time %q", m1.Frames[0].MediaTime)
	}
}

func TestVerifyBlock(t *testing.T) {
	dir := t.TempDir()
	man, err := RenderBlock(dir, testBlock(), clock.Rate2997, true, "seed")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := VerifyBlock(dir, 0, 0, man.SHA256)
	if err != nil || !ok {
		t.Fatalf("verify failed: %v %v", ok, err)
	}
	ok, _ = VerifyBlock(dir, 0, 0, "00")
	if ok {
		t.Fatal("verify accepted wrong checksum")
	}
	// Corrupt one frame file: the block must become unsafe.
	fp := filepath.Join(dir, "out", "frames", "cam00", "block_0000", "frame_000003.pgm")
	data, _ := os.ReadFile(fp)
	data[len(data)-1] ^= 0xff
	os.WriteFile(fp, data, 0o644)
	ok, _ = VerifyBlock(dir, 0, 0, man.SHA256)
	if ok {
		t.Fatal("verify accepted corrupted frame")
	}
}

func TestAggregate(t *testing.T) {
	dir := t.TempDir()
	m1, _ := RenderBlock(dir, testBlock(), clock.Rate2997, true, "seed")
	blk2 := testBlock()
	blk2.Index = 1
	blk2.StartFrame, blk2.EndFrame = 8, 16
	m2, _ := RenderBlock(dir, blk2, clock.Rate2997, true, "seed")
	sha, total := Aggregate([]*Manifest{m1, m2})
	if total != 16 || len(sha) != 64 {
		t.Fatalf("aggregate: %d %q", total, sha)
	}
}
