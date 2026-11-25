package sample

import (
	"mediad/internal/codec"
	"mediad/internal/silence"
	"mediad/internal/timeline"
)

const (
	AUCount    = 128
	GOPFrames  = 32
	DurationNS = int64(4_000_000_000)
	// TruncateAU is the deterministic AU corrupted by the truncate injection.
	TruncateAU = 37
	// DuplicateAU / DuplicateLikeAU contain byte-identical payload.
	DuplicateAU     = 10
	DuplicateLikeAU = 11
)

// Config controls deterministic sample generation.
type Config struct {
	Seed      int64
	TargetBPS int
	DriftPPM  int
	Wrap      bool
	Truncate  bool
}

// Stream is one codec's elementary blob and AU index.
type Stream struct {
	Codec   codec.Codec
	Blob    []byte
	Indexes []timeline.Index
	Config  []byte
}

// Media is the complete self-generated demo fixture.
type Media struct {
	Config
	Streams map[codec.Codec]*Stream
	PCM     []byte
	Gaps    []silence.Gap
}

func roundDiv(num, den int64) int64 {
	if num >= 0 {
		return (num + den/2) / den
	}
	return -((-num + den/2) / den)
}

// Generate builds all three codec streams and the PCM fixture.
func Generate(cfg Config) (*Media, error) {
	if cfg.TargetBPS <= 0 {
		cfg.TargetBPS = 900_000
	}
	if _, err := timeline.NewDriftCorrector(cfg.DriftPPM); err != nil {
		return nil, err
	}
	pcm, gaps := AudioFixture()
	m := &Media{Config: cfg, Streams: make(map[codec.Codec]*Stream, 3), PCM: pcm, Gaps: gaps}
	for _, c := range []codec.Codec{codec.H264, codec.H265, codec.AV1} {
		s, err := generateStream(c, cfg)
		if err != nil {
			return nil, err
		}
		m.Streams[c] = s
	}
	return m, nil
}

func sizeSchedule(targetBPS int) []int {
	totalBytes := int(int64(targetBPS) * DurationNS / timeline.NSPerSecond / 8)
	keyCount := AUCount / GOPFrames
	keyTotal := totalBytes / 24 * keyCount
	keySize := keyTotal / keyCount
	interCount := AUCount - keyCount
	interBudget := totalBytes - keySize*keyCount
	base := interBudget / interCount
	rem := interBudget - base*interCount
	sizes := make([]int, AUCount)
	for i := range sizes {
		if i%GOPFrames == 0 {
			sizes[i] = keySize
		} else {
			sizes[i] = base
			if rem > 0 {
				sizes[i]++
				rem--
			}
		}
	}
	return sizes
}

func generateStream(c codec.Codec, cfg Config) (*Stream, error) {
	sizes := sizeSchedule(cfg.TargetBPS)
	s := &Stream{Codec: c}
	var elapsedTicks int64
	baseTicks := int64(0)
	if cfg.Wrap {
		baseTicks = int64(timeline.PTSTicks) - 4
	}
	for i := 0; i < AUCount; i++ {
		key := i%GOPFrames == 0
		// Alternate 25 ms and 37.5 ms frames; each 32-frame GOP totals 1 s.
		durTicks := int64(2250)
		if i%2 == 1 {
			durTicks = 3375
		}
		driftDen := int64(1_000_000 + cfg.DriftPPM)
		driftElapsed := roundDiv(elapsedTicks*driftDen, 1_000_000)
		unwrapped := baseTicks + driftElapsed
		pts33 := uint64(unwrapped % int64(timeline.PTSTicks))

		contentIndex := i
		if i == DuplicateLikeAU {
			contentIndex = DuplicateAU
		}
		var au []byte
		switch c {
		case codec.H264:
			au = H264AU(key, sizes[i], cfg.Seed, contentIndex)
		case codec.H265:
			au = H265AU(key, sizes[i], cfg.Seed, contentIndex)
		case codec.AV1:
			au = AV1AU(key, sizes[i], cfg.Seed, contentIndex)
		}
		idx := timeline.Index{
			Offset:     int64(len(s.Blob)),
			Length:     len(au),
			PTS33:      pts33,
			DurationNS: timeline.TicksToNS(durTicks),
			Keyframe:   key,
		}
		s.Indexes = append(s.Indexes, idx)
		s.Blob = append(s.Blob, au...)
		if i == 0 {
			s.Config = extractConfig(c, au)
		}
		elapsedTicks += durTicks
	}
	if cfg.Truncate {
		// Claim more bytes than the blob contains while leaving later absolute
		// offsets untouched; ValidateStream isolates exactly this AU.
		s.Indexes[TruncateAU].Length += 256 << 10
	}
	return s, nil
}

func extractConfig(c codec.Codec, au []byte) []byte {
	switch c {
	case codec.H264, codec.H265:
		return codec.ExtractAnnexBConfig(c, au)
	case codec.AV1:
		return codec.ExtractAV1Config(au)
	default:
		return nil
	}
}
