// Package fixture generates self-contained archival WAV, clock, and checksum
// fixtures. No sample files are read from outside the repository.
package fixture

import (
	"os"
	"path/filepath"

	"archivectl/internal/core"
	"archivectl/internal/verify"
	"archivectl/internal/wavscan"
)

// Config controls deterministic fixture generation.
type Config struct {
	Dir              string
	Seed             int64
	Frames           int
	Channels         int
	SampleRate       int
	Bits             int
	Injections       []string
	KeyframeInterval int
	TruncateFrames   int
}

// Manifest records generated paths and expected damage/keyframe facts.
type Manifest struct {
	Config
	InjectionSet      map[string]bool `json:"injection_set"`
	NormalWAV         string          `json:"normal_wav"`
	DamagedWAV        string          `json:"damaged_wav"`
	BaselineWAV       string          `json:"baseline_wav"`
	ClockJSON         string          `json:"clock_json"`
	ChecksJSON        string          `json:"checks_json"`
	ManifestJSON      string          `json:"manifest_json"`
	BlockLengths      []int           `json:"block_lengths"`
	ExpectedKeyframes int             `json:"expected_keyframes"`
	Damage            []core.Span     `json:"damage"`
	ScanError         string          `json:"scan_error,omitempty"`
}

// Generate creates all fixtures in cfg.Dir.
func Generate(cfg Config) (*Manifest, error) {
	if cfg.Dir == "" {
		return nil, core.New(core.KindInvalidArgument, "fixture.Generate", "dir is required")
	}
	if cfg.Frames <= 0 {
		cfg.Frames = 196608
	}
	if cfg.Channels <= 0 {
		cfg.Channels = 6
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 48000
	}
	if cfg.Bits == 0 {
		cfg.Bits = 16
	}
	if cfg.KeyframeInterval <= 0 {
		cfg.KeyframeInterval = 2048
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, core.Wrap(core.KindIO, "fixture.Generate", cfg.Dir, err)
	}
	inj := toSet(cfg.Injections)
	truncateFrames := cfg.TruncateFrames
	if inj["truncate"] && truncateFrames <= 0 {
		truncateFrames = 12288
	}
	frameBytes := cfg.Channels * (cfg.Bits / 8)
	lengths := variableBlockLengths(cfg.Frames, inj["vfr"])
	blockStarts := starts(lengths)

	pcm, err := EncodePCM(cfg.Seed, cfg.Frames, cfg.Channels, cfg.Bits)
	if err != nil {
		return nil, err
	}
	cues := keyframeCues(cfg.Frames, cfg.KeyframeInterval)
	damagedCues := append([]Cue(nil), cues...)
	if inj["missing-cue"] && len(damagedCues) > 8 {
		damagedCues = append(damagedCues[:24], damagedCues[25:]...)
	}
	if inj["duplicate-cue"] && len(damagedCues) > 2 {
		damagedCues = append(damagedCues, damagedCues[2])
	}

	clockFile := buildClock(cfg, inj, lengths, blockStarts, cues)
	checks := buildChecks(pcm, lengths, blockStarts, frameBytes)

	normalPath := filepath.Join(cfg.Dir, "normal.wav")
	damagedPath := filepath.Join(cfg.Dir, "damaged.wav")
	baselinePath := filepath.Join(cfg.Dir, "baseline-repaired.wav")
	clockPath := filepath.Join(cfg.Dir, "clock-records.json")
	checksPath := filepath.Join(cfg.Dir, "block-checks.json")
	manifestPath := filepath.Join(cfg.Dir, "manifest.json")

	wopt := writerOptions{SampleRate: cfg.SampleRate, Channels: cfg.Channels, Bits: cfg.Bits, Cues: cues, IncludeUnknown: true}
	if err := WriteWAV(normalPath, pcm, wopt); err != nil {
		return nil, err
	}

	damagedPCM := append([]byte(nil), pcm...)
	badIdx := 10
	if badIdx >= len(lengths) {
		badIdx = 0
	}
	var fallbackDamage []core.Span
	if inj["bad-crc"] || inj["bit-flip"] {
		start := blockStarts[badIdx]
		off := int(start)*frameBytes + frameBytes/2
		if off >= 0 && off < len(damagedPCM) {
			damagedPCM[off] ^= 0x01
		}
		fallbackDamage = append(fallbackDamage, core.Span{
			Kind: "bit_flip_or_crc_mismatch", StartFrame: start,
			EndFrame: start + int64(lengths[badIdx]), Recoverable: true, Source: "fixture",
		})
	}
	dopt := wopt
	dopt.Cues = damagedCues
	dopt.OmitUnknownPad = inj["missing-padding"]
	if err := WriteWAV(damagedPath, damagedPCM, dopt); err != nil {
		return nil, err
	}
	if inj["truncate"] && truncateFrames > 0 {
		st, err := os.Stat(damagedPath)
		if err != nil {
			return nil, core.Wrap(core.KindIO, "fixture.Generate", damagedPath, err)
		}
		cut := int64(truncateFrames * frameBytes)
		if err := os.Truncate(damagedPath, st.Size()-cut); err != nil {
			return nil, core.Wrap(core.KindIO, "fixture.Generate", damagedPath, err)
		}
		fallbackDamage = append(fallbackDamage, core.Span{
			Kind: "truncation", StartFrame: int64(cfg.Frames - truncateFrames), EndFrame: int64(cfg.Frames),
			Recoverable: int64(truncateFrames) <= int64(core.MaxCachedFrames), Source: "fixture",
		})
	}

	var assessed []core.Span
	rep, scanErr := wavscan.ScanPath(damagedPath)
	if scanErr != nil {
		if !core.AsKind(scanErr, core.KindPadding) {
			return nil, scanErr
		}
	} else {
		checksBlocks := make([]verify.BlockCheck, len(checks.Blocks))
		copy(checksBlocks, checks.Blocks)
		_, spans, err := verify.CheckBlocksPath(damagedPath, rep.DataOffset, frameBytes, rep.AvailableFrames, checksBlocks)
		if err != nil {
			return nil, err
		}
		if rep.AvailableFrames < rep.DeclaredFrames {
			spans = append(spans, core.Span{
				Kind: "truncation", StartFrame: rep.AvailableFrames, EndFrame: rep.DeclaredFrames,
				Recoverable: true, Source: "riff-data-length",
			})
		}
		assessed = core.MergeSpans(spans)
	}
	if len(assessed) == 0 {
		assessed = core.MergeSpans(fallbackDamage)
	}
	baselinePCM := append([]byte(nil), pcm...)
	zeroSpans(baselinePCM, assessed, frameBytes, cfg.Frames)
	bopt := wopt
	bopt.OmitUnknownPad = false
	if err := WriteWAV(baselinePath, baselinePCM, bopt); err != nil {
		return nil, err
	}

	m := &Manifest{
		Config:            cfg,
		InjectionSet:      inj,
		NormalWAV:         normalPath,
		DamagedWAV:        damagedPath,
		BaselineWAV:       baselinePath,
		ClockJSON:         clockPath,
		ChecksJSON:        checksPath,
		ManifestJSON:      manifestPath,
		BlockLengths:      lengths,
		ExpectedKeyframes: len(cues),
		Damage:            assessed,
	}
	if scanErr != nil {
		m.ScanError = scanErr.Error()
	}
	if err := writeJSON(clockPath, clockFile); err != nil {
		return nil, core.Wrap(core.KindIO, "fixture.Generate", clockPath, err)
	}
	if err := writeJSON(checksPath, checks); err != nil {
		return nil, core.Wrap(core.KindIO, "fixture.Generate", checksPath, err)
	}
	if err := writeJSON(manifestPath, m); err != nil {
		return nil, core.Wrap(core.KindIO, "fixture.Generate", manifestPath, err)
	}
	return m, nil
}

func zeroSpans(pcm []byte, spans []core.Span, frameBytes, totalFrames int) {
	for _, s := range spans {
		start := int(s.StartFrame) * frameBytes
		end := int(s.EndFrame) * frameBytes
		if start < 0 {
			start = 0
		}
		if end > len(pcm) {
			end = len(pcm)
		}
		if start < end {
			for i := start; i < end; i++ {
				pcm[i] = 0
			}
		}
	}
}
