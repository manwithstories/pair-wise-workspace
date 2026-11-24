package mp4

import (
	"fmt"
	"sort"

	"mediarig/internal/media"
)

// ColorStandard enumerates the explicitly supported color conventions.
type ColorStandard string

const (
	ColorBT601  ColorStandard = "bt601"
	ColorBT709  ColorStandard = "bt709"
	ColorBT2020 ColorStandard = "bt2020"
)

// ColorMetadata is the source value preserved in reports. Numeric values use
// ISO/IEC 23001-8 / H.273 code points.
type ColorMetadata struct {
	Source     string        `json:"source"`
	Standard   ColorStandard `json:"standard"`
	Primaries  uint16        `json:"primaries"`
	Transfer   uint16        `json:"transfer"`
	Matrix     uint16        `json:"matrix"`
	FullRange  bool          `json:"full_range"`
	Valid      bool          `json:"valid"`
	ConflictID string        `json:"conflict_id,omitempty"`
}

// ColorConfig is the adjudication policy. Priority is most-preferred first.
type ColorConfig struct {
	Priority  []ColorStandard `json:"priority"`
	FullRange bool            `json:"full_range"`
}

// DefaultColorConfig prefers HD BT.709, then UHD BT.2020, then SD BT.601.
func DefaultColorConfig() ColorConfig {
	return ColorConfig{Priority: []ColorStandard{ColorBT709, ColorBT2020, ColorBT601}, FullRange: false}
}

// NCLX returns canonical H.273 code points for a supported standard.
func NCLX(s ColorStandard) (primaries, transfer, matrix uint16, err error) {
	switch s {
	case ColorBT709:
		return 1, 1, 1, nil
	case ColorBT2020:
		// 16 is BT.2020 10/12-bit transfer; matrix 9 is BT.2020 NCL.
		return 9, 16, 9, nil
	case ColorBT601:
		return 6, 6, 6, nil
	default:
		return 0, 0, 0, fmt.Errorf("unsupported color standard %q", s)
	}
}

// NormalizeMetadata fills and validates code points for a named standard.
func NormalizeMetadata(m ColorMetadata) (ColorMetadata, error) {
	p, t, mat, err := NCLX(m.Standard)
	if err != nil {
		return m, media.Corrupt(media.StageColor, "%v", err)
	}
	if m.Primaries == 0 {
		m.Primaries = p
	}
	if m.Transfer == 0 {
		m.Transfer = t
	}
	if m.Matrix == 0 {
		m.Matrix = mat
	}
	if m.Primaries != p || m.Matrix != mat || !validTransfer(m.Standard, m.Transfer) {
		m.Valid = false
		return m, media.Corrupt(media.StageColor,
			"illegal color metadata for %s: primaries=%d transfer=%d matrix=%d",
			m.Standard, m.Primaries, m.Transfer, m.Matrix)
	}
	m.Valid = true
	return m, nil
}

func validTransfer(s ColorStandard, transfer uint16) bool {
	if s == ColorBT2020 {
		return transfer == 16 || transfer == 18 || transfer == 14
	}
	_, expected, _, err := NCLX(s)
	return err == nil && transfer == expected
}

// ColorDecision retains all source values and the adjudicated output value.
type ColorDecision struct {
	Config         ColorConfig     `json:"config"`
	Sources        []ColorMetadata `json:"sources"`
	Selected       ColorMetadata   `json:"selected"`
	Conflict       bool            `json:"conflict"`
	DistinctValues []string        `json:"distinct_values"`
	Policy         string          `json:"policy"`
}

// ResolveColor validates configuration/source metadata and applies priority.
// It never guesses when configuration is empty or metadata is illegal.
func ResolveColor(sources []ColorMetadata, cfg ColorConfig) (ColorDecision, error) {
	if len(cfg.Priority) == 0 {
		return ColorDecision{}, media.Policy(media.StageColor, "no color priority configuration")
	}
	rank := map[ColorStandard]int{}
	for i, s := range cfg.Priority {
		if _, _, _, err := NCLX(s); err != nil {
			return ColorDecision{}, media.Policy(media.StageColor, "invalid color configuration: %v", err)
		}
		rank[s] = i
	}
	if len(sources) == 0 {
		return ColorDecision{}, media.Corrupt(media.StageColor, "no source color metadata")
	}

	validated := make([]ColorMetadata, 0, len(sources))
	distinct := map[string]struct{}{}
	for _, src := range sources {
		m, err := NormalizeMetadata(src)
		if err != nil {
			return ColorDecision{Sources: sources}, err
		}
		m.FullRange = cfg.FullRange
		validated = append(validated, m)
		distinct[fmt.Sprintf("%s/%d/%d/%d", m.Standard, m.Primaries, m.Transfer, m.Matrix)] = struct{}{}
	}
	values := make([]string, 0, len(distinct))
	for v := range distinct {
		values = append(values, v)
	}
	sort.Strings(values)

	selected := validated[0]
	conflict := len(distinct) > 1
	for _, m := range validated[1:] {
		if rank[m.Standard] < rank[selected.Standard] {
			selected = m
		}
	}
	canonical, err := NormalizeMetadata(ColorMetadata{
		Source: "adjudicated", Standard: selected.Standard, FullRange: cfg.FullRange,
	})
	if err != nil {
		return ColorDecision{}, err
	}
	canonical.FullRange = cfg.FullRange
	policy := "single-source"
	if conflict {
		policy = "configured-priority"
	}
	return ColorDecision{
		Config:         cfg,
		Sources:        validated,
		Selected:       canonical,
		Conflict:       conflict,
		DistinctValues: values,
		Policy:         policy,
	}, nil
}
