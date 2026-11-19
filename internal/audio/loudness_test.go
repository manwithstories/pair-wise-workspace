package audio

import (
	"math"
	"testing"

	"avpipe/internal/fault"
)

func toneBlocks(frames, blocks int, amp float64) []Block {
	out := make([]Block, blocks)
	for i := range out {
		b := sineBlock(48000, 1, frames, 1000, amp, int64(i*frames))
		out[i] = b
	}
	return out
}

func TestLoudnessRaisesQuietStreamToTarget(t *testing.T) {
	blocks := toneBlocks(480, 4, 0.1)
	out, stats := Loudness(blocks, LoudnessConfig{TargetLUFS: -20, PeakCeiling: 0.98})
	if stats.GainDB <= 0 {
		t.Fatalf("quiet stream not boosted: %+v", stats)
	}
	if math.Abs(stats.LUFSAfter-(-20)) > 0.2 {
		t.Fatalf("after=%0.2f LUFS want -20", stats.LUFSAfter)
	}
	if stats.RMSAfter <= stats.RMSBefore {
		t.Fatalf("RMS did not rise: %+v", stats)
	}
	if stats.PeakAfter > 0.98 {
		t.Fatalf("peak %v exceeds ceiling", stats.PeakAfter)
	}
	if len(out) != len(blocks) {
		t.Fatalf("block count changed: %d -> %d", len(blocks), len(out))
	}
	// Compare a non-zero sample: sine frame 0 is legitimately zero.
	if out[0].Data[1] <= blocks[0].Data[1] {
		t.Fatal("output was not gained")
	}
	if blocks[0].Data[1] == out[0].Data[1] {
		t.Fatal("input block mutated")
	}
}

func TestLoudnessSilenceNotAmplified(t *testing.T) {
	blocks := make([]Block, 3)
	for i := range blocks {
		blocks[i] = sineBlock(48000, 2, 480, 1000, 0, int64(i*480))
		blocks[i].Layout = Layout{Name: "stereo", Channels: []Channel{ChFL, ChFR}}
	}
	out, stats := Loudness(blocks, LoudnessConfig{TargetLUFS: -16})
	if !stats.Silent || stats.GainDB != 0 || stats.RMSAfter != 0 {
		t.Fatalf("silence report %+v", stats)
	}
	for _, b := range out {
		for _, v := range b.Data {
			if v != 0 {
				t.Fatal("silence was amplified")
			}
		}
	}
}

func TestLoudnessMixedSilentBlocksStaySilent(t *testing.T) {
	blocks := toneBlocks(480, 3, 0.2)
	quiet := sineBlock(48000, 1, 480, 1000, 0, 1440)
	blocks = append(blocks, quiet)
	_, stats := Loudness(blocks, LoudnessConfig{TargetLUFS: -18})
	if stats.Silent {
		t.Fatal("stream with tone reported silent")
	}
}

func TestLoudnessPeakLimiting(t *testing.T) {
	blocks := toneBlocks(480, 2, 0.9)
	_, stats := Loudness(blocks, LoudnessConfig{TargetLUFS: 0, PeakCeiling: 0.5})
	if stats.PeakAfter > 0.5+1e-12 {
		t.Fatalf("peak %v over ceiling", stats.PeakAfter)
	}
	if !stats.PeakLimited {
		t.Fatalf("limiter not reported: %+v", stats)
	}
}

func TestValidateLoudnessConfig(t *testing.T) {
	if err := ValidateLoudnessConfig(LoudnessConfig{TargetLUFS: 5}); err == nil {
		t.Fatal("out-of-range target accepted")
	}
	err := ValidateLoudnessConfig(LoudnessConfig{TargetLUFS: -16, PeakCeiling: 1.5})
	if e := fault.Root(err); e == nil || e.Code != fault.CodeInvalidArgument {
		t.Fatalf("want invalid argument, got %v", err)
	}
	if err := ValidateLoudnessConfig(LoudnessConfig{TargetLUFS: -16}); err != nil {
		t.Fatal(err)
	}
}
