package wav

import (
	"bytes"
	"testing"

	"audiorestore/internal/model"
)

func TestRoundTripPCMFormats(t *testing.T) {
	cases := []struct {
		name  string
		rate  uint32
		chans int
		bits  uint16
	}{
		{"8k-mono-16", 8000, 1, 16},
		{"44.1k-stereo-24", 44100, 2, 24},
		{"96k-quad-32", 96000, 4, 32},
		{"192k-8ch-16", 192000, 8, 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := GenerateConfig{
				SampleRate: tc.rate, Channels: tc.chans, BitDepth: tc.bits,
				Frames: 200, BlockFrames: 50, Layout: "l", Color: "c", Seed: 7,
			}
			raw, err := Generate(cfg)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			rd, err := Open(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			f := rd.Format()
			if f.SampleRate != model.SampleRate(tc.rate) ||
				f.Channels != tc.chans || f.BitDepth != tc.bits {
				t.Fatalf("format mismatch: %+v", f)
			}
			log := model.NewErrorLog()
			blocks, err := rd.ReadBlocks(log)
			if err != nil {
				t.Fatalf("read blocks: %v", err)
			}
			if len(blocks) != 4 {
				t.Fatalf("expected 4 blocks, got %d", len(blocks))
			}
			if log.Degraded() {
				t.Fatalf("clean fixture reported defects: %v", log.All())
			}
			dec, err := DecodeBlock(blocks[0], f)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(dec) != tc.chans || len(dec[0]) != 50 {
				t.Fatalf("decoded geometry wrong: %d chans", len(dec))
			}
		})
	}
}

func TestMissingAndCRCDamageMarked(t *testing.T) {
	cfg := GenerateConfig{
		SampleRate: 48000, Channels: 2, BitDepth: 16,
		Frames: 400, BlockFrames: 100, Seed: 11,
		DropBlocks: []int{1}, CorruptBlocks: []int{2},
	}
	raw, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := Open(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	log := model.NewErrorLog()
	blocks, err := rd.ReadBlocks(log)
	if err != nil {
		t.Fatal(err)
	}
	if !blocks[1].Missing || blocks[1].Data != nil {
		t.Fatal("missing block not represented as a gap")
	}
	if !blocks[2].Corrupt {
		t.Fatal("corrupt block not flagged")
	}
	if !log.Has(model.ErrBlockMissing) || !log.Has(model.ErrBlockCRC) {
		t.Fatalf("expected missing and crc log entries, got %v", log.All())
	}
}

func TestTruncationMarkedNotPadded(t *testing.T) {
	cfg := GenerateConfig{
		SampleRate: 48000, Channels: 2, BitDepth: 16,
		Frames: 300, BlockFrames: 100, Seed: 3, TruncateBytes: 7,
	}
	raw, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := Open(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	log := model.NewErrorLog()
	blocks, err := rd.ReadBlocks(log)
	if err != nil {
		t.Fatal(err)
	}
	last := blocks[len(blocks)-1]
	if !last.Truncated || last.Data != nil {
		t.Fatal("truncated tail must be flagged and carry no fabricated data")
	}
	if !log.Has(model.ErrTruncated) {
		t.Fatal("truncation not logged")
	}
}

func TestDeterministicGeneration(t *testing.T) {
	cfg := GenerateConfig{
		SampleRate: 48000, Channels: 2, BitDepth: 24,
		Frames: 500, BlockFrames: 125, Seed: 99, DropBlocks: []int{0},
	}
	a, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("same seed must yield identical bytes")
	}
}

func TestUnsupportedFormatRejected(t *testing.T) {
	cfg := GenerateConfig{
		SampleRate: 48000, Channels: 9, BitDepth: 16,
		Frames: 100, BlockFrames: 50, Seed: 1,
	}
	if _, err := Generate(cfg); err == nil {
		t.Fatal("9-channel carrier must be rejected")
	}
}
