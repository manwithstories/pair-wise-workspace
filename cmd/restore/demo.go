package main

import (
	"fmt"
	"math/rand"

	"audiorestore/internal/model"
	"audiorestore/internal/pipeline"
	"audiorestore/internal/timeline"
	"audiorestore/internal/wav"
)

// demoBlockFrames keeps every fixture at exactly 128 data blocks, which is
// the workload the pipeline P95 target (<100 ms) is measured against.
const (
	demoBlockFrames = 64
	demoDataBlocks  = 128
	demoFrames      = demoBlockFrames * demoDataBlocks
)

// buildDemo renders the in-process carriers and anchors for one case.
// Everything derives from the seed, so equal seeds yield byte-identical
// carriers, fingerprints and summaries.
func buildDemo(o demoOptions) ([]pipeline.Carrier, []timeline.Anchor, pipeline.Config, error) {
	rng := rand.New(rand.NewSource(o.Seed))
	cfg := pipeline.Config{
		WindowFrames: demoBlockFrames * 2, KeepPartial: true,
		SilenceThresh: 100, SilenceMinRun: 8,
	}

	switch o.Case {
	case "clean":
		return cleanCase(o, rng), anchorsFor(o), cfg, nil
	case "damaged":
		return damagedCase(o, rng), anchorsFor(o), cfg, nil
	case "recover":
		return recoverCase(o, rng), anchorsFor(o), cfg, nil
	default:
		return nil, nil, cfg, fmt.Errorf("unknown case %q", o.Case)
	}
}

// cleanCase renders three intact carriers with differing sample rates,
// bit depths and channel counts but a consistent layout, exercising
// cross-rate PTS mapping without triggering degradation.
func cleanCase(o demoOptions, rng *rand.Rand) []pipeline.Carrier {
	specs := []struct {
		id            string
		rate          uint32
		chans         int
		bits          uint16
		layout, color string
		dep           []string
	}{
		{"track-a", 48000, 2, 16, "stereo_lr", "bt709", nil},
		{"track-b", 44100, 1, 24, "mono", "bt709", nil},
		{"track-c", 96000, 4, 32, "quad_lrrs", "bt709", nil},
	}
	out := make([]pipeline.Carrier, 0, len(specs))
	for _, s := range specs {
		raw, _ := wav.Generate(wav.GenerateConfig{
			SampleRate: s.rate, Channels: s.chans, BitDepth: s.bits,
			Frames: demoFrames, BlockFrames: demoBlockFrames,
			Layout: s.layout, Color: s.color, Seed: o.Seed,
		})
		out = append(out, pipeline.Carrier{
			WAV: raw, ID: s.id, DependsOn: s.dep,
			StartFrame: int64(rng.Intn(100)),
		})
	}
	return out
}

// damagedCase renders carriers carrying missing blocks, CRC errors, tail
// truncation and a color-label conflict, with no parity protection:
// every bad block stays unrecovered and the run is reported degraded.
func damagedCase(o demoOptions, rng *rand.Rand) []pipeline.Carrier {
	drop, corrupt := splitIndices(rng, o.Drop, o.Corrupt, demoDataBlocks)
	rawA, _ := wav.Generate(wav.GenerateConfig{
		SampleRate: 48000, Channels: 2, BitDepth: 16,
		Frames: demoFrames, BlockFrames: demoBlockFrames,
		Layout: "stereo_lr", Color: "bt709", Seed: o.Seed,
		DropBlocks: drop, CorruptBlocks: corrupt,
		TruncateBytes: o.TruncateBytes,
	})
	rawB, _ := wav.Generate(wav.GenerateConfig{
		SampleRate: 48000, Channels: 2, BitDepth: 16,
		Frames: demoFrames, BlockFrames: demoBlockFrames,
		Layout: "stereo_lr", Color: "bt2020", Seed: o.Seed,
	})
	return []pipeline.Carrier{
		{WAV: rawA, ID: "track-a", StartFrame: 0},
		{WAV: rawB, ID: "track-b", StartFrame: 0, DependsOn: []string{"track-a"}},
	}
}

// recoverCase renders parity-protected carriers. --parity gives the
// group size and --drop-per-group how many members are lost in each
// group: one loss per group is fully XOR-recovered, two leaves a gap.
func recoverCase(o demoOptions, rng *rand.Rand) []pipeline.Carrier {
	dataPerGroup := o.Parity
	if dataPerGroup <= 0 {
		dataPerGroup = 4
	}
	nGroups := (demoDataBlocks + dataPerGroup - 1) / dataPerGroup
	var drops []int
	for g := 0; g < nGroups; g++ {
		// Extended numbering keeps data blocks at their original index;
		// parity blocks follow after the full PCM range.
		base := g * dataPerGroup
		for k := 0; k < o.DropPerGroup && k < dataPerGroup; k++ {
			drops = append(drops, base+k)
		}
	}
	raw, total, _ := wav.GenerateParity(wav.GenerateConfig{
		SampleRate: 48000, Channels: 2, BitDepth: 16,
		Frames: demoFrames, BlockFrames: demoBlockFrames,
		Layout: "stereo_lr", Color: "bt709", Seed: o.Seed,
		DropBlocks: drops,
	}, dataPerGroup)
	_ = total
	return []pipeline.Carrier{{
		WAV: raw, ID: "track-a", StartFrame: 0, DataPerGroup: dataPerGroup,
	}}
}

// anchorsFor builds video reference points on several reference clocks so
// the sync P95 exercises real cross-rate integer mapping; every residual
// is bounded by half a target sample period (well under the 1000 us cap).
func anchorsFor(o demoOptions) []timeline.Anchor {
	return []timeline.Anchor{
		{ID: "vid-0", RefRate: model.SampleRate(48000), RefSample: 0},
		{ID: "vid-1", RefRate: model.SampleRate(48000), RefSample: 4801},
		// Non-integer positions relative to other clocks force a real
		// integer-rounding residual rather than an exact grid hit.
		{ID: "vid-2", RefRate: model.SampleRate(44100), RefSample: 4411},
		{ID: "vid-3", RefRate: model.SampleRate(96000), RefSample: 9603},
	}
}

// pickIndices chooses n distinct block indices deterministically via rng.
func pickIndices(rng *rand.Rand, n, bound int) []int {
	if n <= 0 || bound == 0 {
		return nil
	}
	if n > bound {
		n = bound
	}
	perm := rng.Perm(bound)
	return append([]int(nil), perm[:n]...)
}

// splitIndices draws disjoint index sets for drops and corruptions from
// one permutation, so the two damage kinds never hit the same block.
func splitIndices(rng *rand.Rand, nDrop, nCorrupt, bound int) (drop, corrupt []int) {
	perm := rng.Perm(bound)
	if nDrop > bound {
		nDrop = bound
	}
	if nDrop < 0 {
		nDrop = 0
	}
	if nCorrupt > bound-nDrop {
		nCorrupt = bound - nDrop
	}
	drop = append(drop, perm[:nDrop]...)
	if nCorrupt > 0 {
		corrupt = append(corrupt, perm[nDrop:nDrop+nCorrupt]...)
	}
	return drop, corrupt
}
