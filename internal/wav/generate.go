package wav

import (
	"bytes"
	"hash/crc32"

	"audiorestore/internal/model"
)

// GenerateConfig drives the in-process fixture generator. All fixtures are
// synthesized deterministically from Seed: the same seed and config always
// yield the same bytes, CRC table and fingerprints.
type GenerateConfig struct {
	SampleRate uint32
	Channels   int
	BitDepth   uint16
	Frames     int
	BlockFrames int
	Layout     string
	Color      string
	// DropBlocks are removed from the payload entirely and listed in the
	// manifest as missing.
	DropBlocks []int
	// CorruptBlocks keep their bytes but are mutated after the CRC table
	// was recorded, producing a CRC mismatch at read time.
	CorruptBlocks []int
	// TruncateBytes shortens the final file, leaving headers declaring
	// the original length so the reader detects a truncated tail.
	TruncateBytes int
	Seed          int64
	// Synth optionally overrides the per-sample generator. frame is the
	// global frame index, ch the channel index.
	Synth func(frame, ch int, seed int64) int
}

// Generate renders a complete WAV carrier with an rsid manifest in memory.
func Generate(cfg GenerateConfig) ([]byte, error) {
	if err := validateGen(cfg); err != nil {
		return nil, err
	}
	synth := cfg.Synth
	if synth == nil {
		synth = SynthTone
	}
	align := cfg.Channels * int(cfg.BitDepth) / 8
	pcm := make([]byte, cfg.Frames*align)
	for frame := 0; frame < cfg.Frames; frame++ {
		for ch := 0; ch < cfg.Channels; ch++ {
			off := (frame*cfg.Channels + ch) * (int(cfg.BitDepth) / 8)
			EncodeSample(pcm[off:off+int(cfg.BitDepth)/8],
				synth(frame, ch, cfg.Seed), cfg.BitDepth)
		}
	}

	nBlocks := (cfg.Frames + cfg.BlockFrames - 1) / cfg.BlockFrames
	crcs := make([]uint32, nBlocks)
	drop := indexSet(cfg.DropBlocks)
	_ = indexSet(cfg.CorruptBlocks)
	for i := 0; i < nBlocks; i++ {
		start := i * cfg.BlockFrames * align
		end := min((i+1)*cfg.BlockFrames*align, len(pcm))
		crcs[i] = crc32.ChecksumIEEE(pcm[start:end])
	}
	// Mutate corrupt blocks after recording the original CRC.
	for _, idx := range cfg.CorruptBlocks {
		if idx < 0 || idx >= nBlocks {
			continue
		}
		start := idx * cfg.BlockFrames * align
		end := min((idx+1)*cfg.BlockFrames*align, len(pcm))
		if start < end {
			pcm[start] ^= 0x5a
		}
	}

	var data bytes.Buffer
	for i := 0; i < nBlocks; i++ {
		if drop[i] {
			continue
		}
		start := i * cfg.BlockFrames * align
		end := min((i+1)*cfg.BlockFrames*align, len(pcm))
		data.Write(pcm[start:end])
	}

	rsid := encodeManifest(cfg, crcs)
	if len(rsid)%2 == 1 {
		rsid = append(rsid, 0) // RIFF word alignment; parser ignores the pad
	}
	var out bytes.Buffer
	writeHeader(&out, cfg, data.Len(), rsid)
	writeSideChunk(&out, ManifestID, rsid)
	writeDataChunk(&out, data.Bytes())

	raw := out.Bytes()
	if cfg.TruncateBytes > 0 && cfg.TruncateBytes < len(raw) {
		raw = raw[:len(raw)-cfg.TruncateBytes]
	}
	return raw, nil
}

// SynthTone is the default deterministic sample source: a per-channel sine
// mixture with a seed-dependent base frequency, plus periodic silence
// windows so the silence detector has real targets.
func SynthTone(frame, ch int, seed int64) int {
	period := 96 // ~200 Hz at 48 kHz nominal block; rate-independent shape
	phase := (frame*2 + ch*7 + int(seed%31)) % period
	quiet := frame%400 < 24 // repeated low-energy windows
	if quiet {
		return 0
	}
	base := int(20000.0 * float64(sinInt(phase, period)) / 1000.0)
	return base + (ch-int(seed%3))*64
}

// sinInt returns a deterministic sine-like lobe in [-1000,1000] using a
// quadratic curve per half period (fixture synthesis only; PTS math stays
// integer-only).
func sinInt(phase, period int) int {
	half := period / 2
	x := phase % period
	sign := 1
	if x >= half {
		x -= half
		sign = -1
	}
	t := 2*float64(x)/float64(half) - 1
	return sign * int((1.0-t*t)*1000)
}

func validateGen(cfg GenerateConfig) error {
	if cfg.Channels < 1 || cfg.Channels > 8 {
		return errGen("channels must be 1..8")
	}
	if cfg.BitDepth != 16 && cfg.BitDepth != 24 && cfg.BitDepth != 32 {
		return errGen("bit depth must be 16/24/32")
	}
	if !model.SampleRate(cfg.SampleRate).Valid() {
		return errGen("sample rate outside 8000..192000")
	}
	if cfg.Frames <= 0 || cfg.BlockFrames <= 0 {
		return errGen("frames and block frames must be positive")
	}
	if len(cfg.Layout) > 255 || len(cfg.Color) > 255 {
		return errGen("layout/color tags must fit in one byte")
	}
	return nil
}

func errGen(msg string) error {
	return model.TrackedError{Kind: model.ErrInvalidParam, Track: "fixture", Msg: msg}
}

func indexSet(idx []int) map[int]bool {
	set := make(map[int]bool, len(idx))
	for _, i := range idx {
		set[i] = true
	}
	return set
}
