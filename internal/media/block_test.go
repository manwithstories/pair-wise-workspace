package media_test

import (
	"testing"

	"media-pipeline/internal/clock"
	"media-pipeline/internal/fault"
	"media-pipeline/internal/media"
)

func TestBlockFrameCount(t *testing.T) {
	tb := clock.Timebase{SampleRate: 48000, Channels: 2, BitsPerSample: 16}
	b := media.Block{ID: "b1", Format: tb, FirstSample: 100, PCM: make([]byte, 40)}
	n, err := b.FrameCount()
	if err != nil || n != 10 {
		t.Fatalf("frames = (%d,%v), want 10,nil", n, err)
	}
	if got := b.EndSample(); got != 110 {
		t.Fatalf("end sample = %d, want 110", got)
	}

	bad := media.Block{ID: "b2", Format: tb, PCM: make([]byte, 41)}
	_, err = bad.FrameCount()
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindMalformedInput || fe.ByteOffset != 40 {
		t.Fatalf("unaligned tail error = %#v, want malformed at byte 40", err)
	}
}
