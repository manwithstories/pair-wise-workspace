package fixture

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"segmanifest/internal/manifest"
)

const FileName = "fixture.json"

type Segment struct {
	Number     uint64 `json:"number"`
	StartUS    int64  `json:"start_us"`
	DurationUS int64  `json:"duration_us"`
}

type Representation struct {
	ID            string    `json:"id"`
	Bandwidth     uint64    `json:"bandwidth"`
	AudioOffsetUS int64     `json:"audio_offset_us"`
	Segments      []Segment `json:"segments"`
}

type Spec struct {
	Seed            int64            `json:"seed"`
	TargetUS        int64            `json:"target_us"`
	JitterUS        int64            `json:"jitter_us"`
	Version         uint64           `json:"version"`
	Template        string           `json:"template"`
	Representations []Representation `json:"representations"`
}

type Options struct {
	Seed            int64
	Representations int
	Segments        int
	TargetUS        int64
	JitterUS        int64
	AudioOffsetUS   int64
	Holes           int
	Version         uint64
}

var catalog = []Representation{
	{ID: "r1080", Bandwidth: 6_000_000},
	{ID: "r720", Bandwidth: 3_000_000},
	{ID: "r480", Bandwidth: 1_500_000},
	{ID: "r360", Bandwidth: 800_000},
	{ID: "r240", Bandwidth: 400_000},
}

func Generate(opts Options) (*Spec, error) {
	if opts.Representations <= 0 || opts.Representations > 12 {
		return nil, manifest.NewError("fixture.representations", "representations must be between 1 and 12")
	}
	if opts.Segments <= 0 {
		return nil, manifest.NewError("fixture.segments", "segments must be positive")
	}
	if opts.TargetUS <= 0 {
		return nil, manifest.NewError("fixture.target", "target duration must be positive")
	}
	if opts.JitterUS < 0 {
		return nil, manifest.NewError("fixture.jitter", "jitter must not be negative")
	}
	if opts.Holes < 0 || opts.Holes >= opts.Segments {
		return nil, manifest.NewError("fixture.holes", "holes must be smaller than segment count")
	}
	if opts.Version == 0 {
		opts.Version = 1
	}
	spec := &Spec{
		Seed:     opts.Seed,
		TargetUS: opts.TargetUS,
		JitterUS: opts.JitterUS,
		Version:  opts.Version,
		Template: manifest.DefaultTemplate,
	}
	holeAt := opts.Segments / 2
	for r := 0; r < opts.Representations; r++ {
		base := catalog[r%len(catalog)]
		rep := Representation{
			ID:            base.ID,
			Bandwidth:     base.Bandwidth,
			AudioOffsetUS: opts.AudioOffsetUS,
		}
		if r >= len(catalog) {
			rep.ID = fmt.Sprintf("r%d", r)
			rep.Bandwidth = uint64(16_000_000 / (r + 1))
		}
		boundaries := make([]int64, opts.Segments+1)
		for i := 1; i < opts.Segments; i++ {
			boundaries[i] = int64(i)*opts.TargetUS + boundaryJitter(opts.Seed, r, i, opts.JitterUS)
		}
		boundaries[opts.Segments] = int64(opts.Segments) * opts.TargetUS
		for i := 0; i < opts.Segments; i++ {
			number := uint64(i + 1)
			if opts.Holes > 0 && i >= holeAt && i < holeAt+opts.Holes {
				continue
			}
			rep.Segments = append(rep.Segments, Segment{
				Number:     number,
				StartUS:    boundaries[i],
				DurationUS: boundaries[i+1] - boundaries[i],
			})
		}
		spec.Representations = append(spec.Representations, rep)
	}
	return spec, nil
}

func boundaryJitter(seed int64, rep, boundary int, jitterUS int64) int64 {
	if jitterUS == 0 || boundary <= 0 {
		return 0
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d", seed, rep, boundary)))
	magnitudeBits := binary.BigEndian.Uint64(digest[:8]) >> 11
	unit := float64(magnitudeBits) / float64(uint64(1)<<53)
	amplitude := float64(jitterUS) * 0.906
	value := math.Pow(unit, 10.6) * amplitude
	if digest[8]&1 == 0 {
		value = -value
	}
	return int64(math.Round(value))
}

func Write(dir string, spec *Spec) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(dir, FileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func Load(dir string) (*Spec, error) {
	file, err := os.Open(filepath.Join(dir, FileName))
	if err != nil {
		return nil, fmt.Errorf("read fixture: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var spec Spec
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("decode fixture: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return nil, err
	}
	if err := spec.validate(); err != nil {
		return nil, err
	}
	return &spec, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return manifest.NewError("fixture.trailing_data", "fixture contains trailing JSON values")
		}
		return fmt.Errorf("decode fixture trailing data: %w", err)
	}
	return nil
}

func (s *Spec) validate() error {
	if s.TargetUS <= 0 {
		return manifest.NewError("fixture.invalid_target", "target duration must be positive")
	}
	if len(s.Representations) == 0 {
		return manifest.NewError("fixture.no_representations", "fixture has no representations")
	}
	ids := make(map[string]bool)
	for _, rep := range s.Representations {
		if rep.ID == "" || ids[rep.ID] {
			return manifest.NewError("fixture.invalid_representation", "missing or duplicate representation ID %q", rep.ID)
		}
		ids[rep.ID] = true
		if rep.Bandwidth == 0 || len(rep.Segments) == 0 {
			return manifest.NewError("fixture.invalid_representation", "representation %q has invalid metadata", rep.ID)
		}
	}
	Sort(s.Representations)
	return nil
}

func Sort(reps []Representation) {
	for i := 1; i < len(reps); i++ {
		for j := i; j > 0 && less(reps[j], reps[j-1]); j-- {
			reps[j], reps[j-1] = reps[j-1], reps[j]
		}
	}
}

func less(a, b Representation) bool {
	if a.Bandwidth != b.Bandwidth {
		return a.Bandwidth < b.Bandwidth
	}
	return a.ID < b.ID
}

func (s *Spec) ToManifest(targetUS, toleranceUS int64) (*manifest.Manifest, error) {
	m := &manifest.Manifest{
		TargetUS:    targetUS,
		ToleranceUS: toleranceUS,
		Template:    s.Template,
		Window:      manifest.Window{Version: s.Version},
	}
	if m.Template == "" {
		m.Template = manifest.DefaultTemplate
	}
	for _, rep := range s.Representations {
		out := manifest.Representation{ID: rep.ID, Bandwidth: rep.Bandwidth, AudioOffsetUS: rep.AudioOffsetUS}
		for _, seg := range rep.Segments {
			out.Segments = append(out.Segments, manifest.Segment{Number: seg.Number, StartUS: seg.StartUS, DurationUS: seg.DurationUS})
		}
		m.Representations = append(m.Representations, out)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// Merge combines source periods in ascending version order. Overlapping
// segments are taken from the newest period. A disagreement inside one version
// is a content conflict rather than an ambiguous override.
func Merge(periods ...*Spec) (*Spec, error) {
	if len(periods) == 0 {
		return nil, manifest.NewError("merge.no_periods", "no fixture periods supplied")
	}
	sorted := append([]*Spec(nil), periods...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })
	type mergedRep struct {
		rep      Representation
		version  uint64
		segments map[uint64]Segment
	}
	reps := make(map[string]*mergedRep)
	var newest *Spec
	for _, period := range sorted {
		if period.Version == 0 {
			return nil, manifest.NewError("merge.invalid_version", "period version must be positive")
		}
		if newest == nil || period.Version >= newest.Version {
			newest = period
		}
		for _, source := range period.Representations {
			existing, ok := reps[source.ID]
			if !ok {
				existing = &mergedRep{rep: source, version: period.Version, segments: make(map[uint64]Segment)}
				for _, segment := range source.Segments {
					existing.segments[segment.Number] = segment
				}
				reps[source.ID] = existing
				continue
			}
			if existing.rep.Bandwidth != source.Bandwidth {
				return nil, manifest.NewError("merge.representation_mismatch", "bandwidth conflicts for %q", source.ID)
			}
			if period.Version <= existing.version && existing.rep.AudioOffsetUS != source.AudioOffsetUS {
				return nil, manifest.NewError("merge.content_conflict", "conflicting audio offset for %q at version %d", source.ID, period.Version)
			}
			for _, segment := range source.Segments {
				if previous, exists := existing.segments[segment.Number]; exists && period.Version <= existing.version && !sameSegment(previous, segment) {
					return nil, manifest.NewError("merge.content_conflict", "conflicting fixture segment %q/%d at version %d", source.ID, segment.Number, period.Version)
				}
				existing.segments[segment.Number] = segment
			}
			existing.version = period.Version
			existing.rep.AudioOffsetUS = source.AudioOffsetUS
		}
	}
	out := &Spec{Seed: newest.Seed, TargetUS: newest.TargetUS, JitterUS: newest.JitterUS, Version: newest.Version, Template: newest.Template}
	for _, merged := range reps {
		rep := merged.rep
		rep.Segments = make([]Segment, 0, len(merged.segments))
		for _, segment := range merged.segments {
			rep.Segments = append(rep.Segments, segment)
		}
		sort.Slice(rep.Segments, func(i, j int) bool { return rep.Segments[i].Number < rep.Segments[j].Number })
		out.Representations = append(out.Representations, rep)
	}
	Sort(out.Representations)
	if err := out.validate(); err != nil {
		return nil, err
	}
	return out, nil
}

func sameSegment(a, b Segment) bool {
	return a.Number == b.Number && a.StartUS == b.StartUS && a.DurationUS == b.DurationUS
}

func SplitPaths(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := parts[:0]
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
