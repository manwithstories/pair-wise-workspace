package timeline

import (
	"errors"
	"testing"

	"avcheck/internal/clock"
)

const framesPerBlock = clock.SampleRate / clock.FrameRate // 1600, one frame edge

func trackBlock(id uint32, seq uint16, ch, value int) *clock.Block {
	return &clock.Block{
		TrackID: id, Seq: seq, Channels: ch,
		StartTick: int64(seq) * framesPerBlock,
		Samples:   make([]int32, framesPerBlock*ch),
		Source:    clock.SourceOriginal,
	}
}

func filledTrack(id uint32, ch, blocks int, value int32) Track {
	tr := Track{ID: id, Channels: ch}
	for s := 0; s < blocks; s++ {
		b := trackBlock(id, uint16(s), ch, int(value))
		for i := range b.Samples {
			b.Samples[i] = value
		}
		tr.Blocks = append(tr.Blocks, b)
	}
	return tr
}

func TestMixAlignedTracksAndClipping(t *testing.T) {
	a := filledTrack(1, 2, 3, 1000)
	b := filledTrack(2, 2, 3, 2000)
	m, err := Mix([]Track{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if m.Frames != 3*framesPerBlock || len(m.Samples) != m.Frames*2 {
		t.Fatalf("frames=%d samples=%d", m.Frames, len(m.Samples))
	}
	if m.Samples[0] != 3000 {
		t.Fatalf("mix value=%d, want 3000", m.Samples[0])
	}
	if m.PerTrackFrames[1] != 3*framesPerBlock || m.PerTrackFrames[2] != 3*framesPerBlock {
		t.Fatalf("per-track frames=%+v", m.PerTrackFrames)
	}

	// Saturating clipping on overflow.
	hot := filledTrack(3, 2, 1, 2_000_000_000)
	cold := filledTrack(4, 2, 1, 2_000_000_000)
	m2, err := Mix([]Track{hot, cold})
	if err != nil {
		t.Fatal(err)
	}
	if m2.Samples[0] != 2147483647 {
		t.Fatalf("clipped value=%d, want MaxInt32", m2.Samples[0])
	}
}

func TestSyncErrorOnDriftBeyondOneSample(t *testing.T) {
	a := filledTrack(1, 1, 2, 1)
	b := filledTrack(2, 1, 2, 1)
	// Shift track b's blocks by two ticks -> exceeds the 1-sample tolerance.
	for _, blk := range b.Blocks {
		blk.StartTick += 2
	}
	_, err := Mix([]Track{a, b})
	var ce *clock.Error
	if !errors.As(err, &ce) || ce.Kind != clock.KindSync {
		t.Fatalf("err=%v, want sync_error", err)
	}
}

func TestOneSampleDriftAcceptedAndFrameAligned(t *testing.T) {
	a := filledTrack(1, 1, 2, 10)
	b := filledTrack(2, 1, 2, 10)
	for _, blk := range b.Blocks {
		blk.StartTick += 1 // whole-track shift of exactly one tick: tolerated
	}
	m, err := Mix([]Track{a, b})
	if err != nil {
		t.Fatalf("1-tick drift must be tolerated, got %v", err)
	}
	if m.Frames%framesPerBlock != 0 {
		t.Fatalf("output %d frames not on 30fps boundary", m.Frames)
	}
}

func TestGapInsideTrackRejected(t *testing.T) {
	tr := filledTrack(1, 1, 3, 1)
	tr.Blocks[1].StartTick += framesPerBlock // opens a one-block gap
	_, err := Mix([]Track{tr})
	var ce *clock.Error
	if !errors.As(err, &ce) || ce.Kind != clock.KindSync {
		t.Fatalf("err=%v, want sync_error for internal gap", err)
	}
}

func TestDeclaredOffsetApplied(t *testing.T) {
	a := filledTrack(1, 1, 2, 7)
	b := filledTrack(2, 1, 2, 7)
	b.OffsetTicks = 0 // same start after offset
	m, err := Mix([]Track{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if m.Samples[0] != 14 {
		t.Fatalf("offset mix value=%d, want 14", m.Samples[0])
	}
}
