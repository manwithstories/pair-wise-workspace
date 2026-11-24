package pipeline

import (
	"fmt"
	"math/rand"

	"mediaflow/internal/cmaf"
	"mediaflow/internal/gen"
)

// Config is the full demo configuration. Every random choice is derived from
// Seed, so the same Config produces the same fixtures and summaries.
type Config struct {
	Seed            int64
	Representations int
	Loss            float64
	TruncateEvery   int

	DurationSec     int
	FPS             int
	FragmentSec     int
	ReorderWindow   int
	MaxSegmentBytes int
	BoundaryTolMS   float64
	MismatchTolMS   float64
	SyncTolMS       float64
	MinGapUs        int64
	BaseDelayUs     int64
	ReorderFraction float64
	DuplicateFrac   float64
}

// DefaultConfig returns the acceptance configuration for the demo command.
func DefaultConfig() Config {
	return Config{
		Seed:            7,
		Representations: 3,
		Loss:            0.03,
		TruncateEvery:   11,
		DurationSec:     30,
		FPS:             30,
		FragmentSec:     1,
		ReorderWindow:   64,
		MaxSegmentBytes: 4 << 20,
		BoundaryTolMS:   40,
		MismatchTolMS:   5,
		SyncTolMS:       40,
		MinGapUs:        1_000,
		BaseDelayUs:     5_000,
		ReorderFraction: 0.05,
		DuplicateFrac:   0.01,
	}
}

// Validate checks configuration bounds before any work is done.
func (c Config) Validate() error {
	switch {
	case c.Representations < 1 || c.Representations > 8:
		return fmt.Errorf("representations must be 1..8, got %d", c.Representations)
	case c.Loss < 0 || c.Loss > 0.5:
		return fmt.Errorf("loss must be within 0..0.5, got %v", c.Loss)
	case c.TruncateEvery < 0:
		return fmt.Errorf("truncate-every must be >= 0")
	case c.DurationSec < 1 || c.DurationSec > 120:
		return fmt.Errorf("duration must be 1..120s, got %d", c.DurationSec)
	case c.FPS < 1 || c.FPS > 120:
		return fmt.Errorf("fps must be 1..120, got %d", c.FPS)
	case c.FragmentSec < 1 || c.FragmentSec > c.DurationSec:
		return fmt.Errorf("fragment duration must be 1..duration")
	case c.ReorderWindow < 4 || c.ReorderWindow > 4096:
		return fmt.Errorf("reorder window must be 4..4096, got %d", c.ReorderWindow)
	case c.MaxSegmentBytes < 1024:
		return fmt.Errorf("max segment bytes must be >= 1024")
	}
	return nil
}

// ladderEntry is one rung of the bitrate ladder.
type ladderEntry struct {
	name      string
	width     uint16
	height    uint16
	timescale uint32
	sampleSz  uint32
}

var ladder = []ladderEntry{
	{"360p", 640, 360, 90000, 512},
	{"720p", 1280, 720, 90000, 1024},
	{"1080p", 1920, 1080, 90000, 2048},
}

// DamageKind names the injected defect for one fragment.
type DamageKind string

const (
	DamageNone       DamageKind = ""
	DamageTruncate   DamageKind = "truncate"
	DamageBoxCorrupt DamageKind = "box_corrupt"
	DamageSkip       DamageKind = "mfhd_gap"
	DamageDuplicate  DamageKind = "duplicate"
	DamageTfdtReg    DamageKind = "tfdt_regression"
)

// Source is one representation's generated input, ready for scanning.
type Source struct {
	Name      string
	TrackID   uint32
	Timescale uint32
	Width     uint16
	Height    uint16
	Init      []byte
	Segments  []SegmentInput
	Truth     []Injection
}

// SegmentInput is one generated media segment with its ground-truth damage.
type SegmentInput struct {
	Index  int
	Bytes  []byte
	Damage DamageKind
	Note   string
}

// Injection records a planted defect.
type Injection struct {
	Segment int
	Kind    DamageKind
	Note    string
}

// BuildSources generates every representation from cfg. It is pure with
// respect to cfg.Seed.
func BuildSources(cfg Config) ([]Source, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	sources := make([]Source, 0, cfg.Representations)
	for i := 0; i < cfg.Representations; i++ {
		sources = append(sources, buildOne(cfg, i))
	}
	return sources, nil
}

func buildOne(cfg Config, rep int) Source {
	le := ladder[rep%len(ladder)]
	samplesPerFrag := cfg.FPS * cfg.FragmentSec
	fragments := cfg.DurationSec / cfg.FragmentSec
	sampleDelta := le.timescale / uint32(cfg.FPS)
	if sampleDelta == 0 {
		sampleDelta = 1
	}
	trackID := uint32(rep + 1)
	rng := rand.New(rand.NewSource(cfg.Seed*7919 + int64(rep)*104729))
	plan := damagePlan(cfg, rep, rng, fragments)

	// Representation 3 starts 20ms into the shared timeline and drops one
	// trailing frame: a genuine cross-node boundary disagreement that stays
	// inside the sync tolerance but is still reported.
	offsetTicks := uint64(0)
	lastExtraSamples := 0
	if rep == 2 && fragments > 2 {
		offsetTicks = uint64(le.timescale) * 20 / 1000
		lastExtraSamples = -1
	}

	mux := gen.NewMuxer(gen.TrackSpec{
		TrackID: trackID, Timescale: le.timescale, Width: le.width, Height: le.height,
		SampleDelta: sampleDelta, SampleSize: le.sampleSz, KeyframeEvery: samplesPerFrag,
	})
	src := Source{
		Name: le.name, TrackID: trackID, Timescale: le.timescale,
		Width: le.width, Height: le.height, Init: mux.Init(),
	}
	seq := uint32(1)
	base := offsetTicks
	for f := 0; f < fragments; f++ {
		kind := plan[f]
		nSamples := samplesPerFrag
		if f == fragments-1 && lastExtraSamples < 0 {
			nSamples += lastExtraSamples
		}
		fs := gen.FragmentSpec{Sequence: seq, BaseTicks: base, Samples: nSamples, Keyframes: []int{0}}
		note := ""
		switch kind {
		case DamageSkip:
			note = fmt.Sprintf("fragment %d omitted, sequence advances", f)
			base += uint64(sampleDelta) * uint64(samplesPerFrag)
			seq++
			src.Truth = append(src.Truth, Injection{Segment: f, Kind: kind, Note: note})
			continue
		case DamageTfdtReg:
			note = "tfdt rewound one fragment"
		case DamageDuplicate:
			note = "fragment emitted twice with the same mfhd"
		case DamageTruncate:
			note = "segment cut inside the last mdat"
		case DamageBoxCorrupt:
			note = "mdat declared size corrupted"
		}
		fragSpec := fs
		if kind == DamageTfdtReg && base > 2*uint64(sampleDelta)*uint64(samplesPerFrag) {
			// Rewind only this fragment's tfdt (two fragments back, so it is
			// strictly below the previous start); the following fragment
			// resumes the correct timeline so one bad box cannot shift the
			// whole layer.
			fragSpec.BaseTicks = base - 2*uint64(sampleDelta)*uint64(samplesPerFrag)
		}
		segBytes, _ := mux.BuildFragment(fragSpec)
		switch kind {
		case DamageTruncate:
			segBytes = segBytes[:len(segBytes)*3/4]
		case DamageBoxCorrupt:
			segBytes = corruptFirstMdat(segBytes)
		}
		src.Segments = append(src.Segments, SegmentInput{Index: f, Bytes: segBytes, Damage: kind, Note: note})
		src.Truth = append(src.Truth, Injection{Segment: f, Kind: kind, Note: note})
		if kind == DamageDuplicate {
			src.Segments = append(src.Segments, SegmentInput{
				Index: f, Bytes: segBytes, Damage: kind, Note: "duplicate copy",
			})
		}
		base += uint64(sampleDelta) * uint64(samplesPerFrag)
		seq++
	}
	return src
}

// damagePlan returns one damage kind per fragment from the seeded stream. The
// first fragment always stays clean so every layer has a trustworthy anchor.
func damagePlan(cfg Config, rep int, rng *rand.Rand, fragments int) map[int]DamageKind {
	plan := make(map[int]DamageKind, fragments)
	if cfg.TruncateEvery > 0 {
		for f := cfg.TruncateEvery - 1; f < fragments; f += cfg.TruncateEvery {
			plan[f] = DamageTruncate
		}
	}
	if fragments < 12 {
		return plan
	}
	switch rep % 3 {
	case 0:
		plan[fragments/2] = DamageBoxCorrupt
		if d := 4 + rng.Intn(fragments/2); d != fragments/2 {
			plan[d] = DamageDuplicate
		}
	case 1:
		gap := fragments/4 + 1
		if _, taken := plan[gap]; !taken {
			plan[gap] = DamageSkip
		}
		reg := fragments*3/4 + 1
		if _, taken := plan[reg]; !taken {
			plan[reg] = DamageTfdtReg
		}
	case 2:
		d := fragments/3 + 1
		if _, taken := plan[d]; !taken {
			plan[d] = DamageDuplicate
		}
	}
	return plan
}

// corruptFirstMdat shrinks the first mdat's declared size to its header, so
// the payload bytes are no longer inside any box. The declared length stays
// plausible (it fits the segment), making this local corruption rather than
// truncation.
func corruptFirstMdat(seg []byte) []byte {
	cp := make([]byte, len(seg))
	copy(cp, seg)
	for off := 0; off+8 <= len(cp); {
		b, err := cmaf.ParseBoxHeader(cp, off, len(cp))
		if err != nil || !b.Valid {
			break
		}
		if b.Type == "mdat" {
			cp[b.Offset] = 0
			cp[b.Offset+1] = 0
			cp[b.Offset+2] = 0
			cp[b.Offset+3] = 8
			return cp
		}
		off = b.End
	}
	return cp
}
