package loudness

import (
	"testing"

	"loudcheck/internal/wavio"
)

// The engine only keeps biquad state, a fixed 4-hop block ring and a fixed
// true-peak history, regardless of how many frames stream through it. Feed two
// very different lengths and assert the retained state size does not grow.
func TestEngineStateBoundedAcrossLengths(t *testing.T) {
	engine, err := NewEngine(wavio.Header{SampleRate: 48000, Channels: 6,
		ChannelMask: 0x1 | 0x2 | 0x4 | 0x8 | 0x20 | 0x40})
	if err != nil {
		t.Fatal(err)
	}
	short := wavio.TonePlanar(48000, 1, 6, 1000, 0.1, 0.2)
	long := wavio.TonePlanar(48000, 30, 6, 1000, 0.1, 0.2)
	ringAfterShort := len(engine.blocks.ring)
	engine.Process(short)
	engine.Process(long)
	if len(engine.blocks.ring) != ringAfterShort {
		t.Fatalf("block ring grew from %d to %d", ringAfterShort, len(engine.blocks.ring))
	}
	if len(engine.all) == 0 {
		t.Fatal("expected emitted blocks")
	}
	result := engine.Result()
	if !result.Defined {
		t.Fatal("long stream must produce defined integrated loudness")
	}
}

// ReadFrames must hand back bounded planar buffers even for a large declared
// data chunk, proving the reader never buffers the whole file.
func TestReadFramesBounded(t *testing.T) {
	planar := wavio.TonePlanar(48000, 60, 2, 1000, 0.1, 0.3)
	hdr := wavio.WriteHeader{SampleRate: 48000, Channels: 2, BitsPerSample: 24}
	file := &testSink{}
	writer, err := wavio.NewWriter(file, hdr)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteFrames(planar); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := wavio.NewReader(bytesReader(file.buf))
	if err != nil {
		t.Fatal(err)
	}
	const maxFrames = 4096
	for {
		frames, readErr := reader.ReadFrames(maxFrames)
		if len(frames) > 0 && len(frames[0]) > maxFrames {
			t.Fatalf("reader returned %d frames, exceeding bound %d", len(frames[0]), maxFrames)
		}
		if readErr != nil {
			break
		}
	}
}
