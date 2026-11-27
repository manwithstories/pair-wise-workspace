// Package layout maps interleaved audio between channel layouts. The mapping
// is a fixed matrix: every output channel is a weighted sum of the input
// channels. Unknown channel labels are always rejected.
package layout

import (
	"fmt"
	"strings"

	"avpipe/internal/media"
)

// Layout is a named, ordered set of channel labels.
type Layout struct {
	Name   string
	Labels []string
}

// Canonical layouts. 5.1 uses the WAVE order L, R, C, LFE, Ls, Rs.
var (
	Mono     = Layout{Name: "mono", Labels: []string{"M"}}
	Stereo   = Layout{Name: "stereo", Labels: []string{"L", "R"}}
	Surround = Layout{Name: "5.1", Labels: []string{"L", "R", "C", "LFE", "Ls", "Rs"}}
)

// known lists every layout that may appear as a source or target.
var known = []Layout{Mono, Stereo, Surround}

// ByName resolves a layout name.
func ByName(name string) (Layout, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	for _, l := range known {
		if l.Name == key {
			return l, nil
		}
	}
	return Layout{}, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "layout",
		fmt.Errorf("unknown layout %q", name))
}

// ForLabels finds the layout matching exactly the given channel labels.
func ForLabels(labels []string) (Layout, error) {
	for _, l := range known {
		if len(l.Labels) != len(labels) {
			continue
		}
		match := true
		for i := range labels {
			if l.Labels[i] != labels[i] {
				match = false
				break
			}
		}
		if match {
			return l, nil
		}
	}
	return Layout{}, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "layout",
		fmt.Errorf("unknown channel labels %v", labels))
}

// Matrix maps a fixed source label set onto a target layout.
type Matrix struct {
	Src   []string
	Dst   Layout
	coeff [][]float64 // [dst channel][src channel]
}

// BuildMatrix validates the source labels against the catalogue and their
// mapping onto dst. Every source label must be known and unique, and a target
// layout larger than 5.1 is refused.
func BuildMatrix(src []string, dst Layout) (*Matrix, error) {
	if len(src) == 0 {
		return nil, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "matrix",
			fmt.Errorf("empty source labels"))
	}
	seen := make(map[string]bool, len(src))
	for _, label := range src {
		if label == "" {
			return nil, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "matrix",
				fmt.Errorf("empty channel label"))
		}
		if seen[label] {
			return nil, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "matrix",
				fmt.Errorf("duplicate channel label %q", label))
		}
		seen[label] = true
		if !isKnownLabel(label) {
			return nil, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "matrix",
				fmt.Errorf("unknown channel label %q", label))
		}
	}
	if _, err := ForLabels(dst.Labels); err != nil {
		return nil, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "matrix",
			fmt.Errorf("unknown target layout %q", dst.Name))
	}
	if !seen["L"] && !seen["R"] && !seen["M"] && len(src) > 1 {
		return nil, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "matrix",
			fmt.Errorf("source labels %v carry no program channel", src))
	}
	coeff := make([][]float64, len(dst.Labels))
	for d := range coeff {
		coeff[d] = make([]float64, len(src))
	}
	for d, target := range dst.Labels {
		for s, source := range src {
			coeff[d][s] = coefficient(source, target)
		}
		if allZero(coeff[d]) {
			return nil, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "matrix",
				fmt.Errorf("target channel %q has no source", target))
		}
	}
	return &Matrix{Src: append([]string(nil), src...), Dst: dst, coeff: coeff}, nil
}

func isKnownLabel(label string) bool {
	for _, l := range known {
		for _, known := range l.Labels {
			if known == label {
				return true
			}
		}
	}
	return false
}

func allZero(v []float64) bool {
	for _, x := range v {
		if x != 0 {
			return false
		}
	}
	return true
}

// coefficient is the fixed downmix gain from one source channel to one target
// channel. Equal-power (-3 dB) gains are used where energy is split, and the
// LFE channel is deliberately excluded from the program mix.
func coefficient(source, target string) float64 {
	const minus3 = 0.7071067811865476
	switch target {
	case "M":
		switch source {
		case "M":
			return 1
		case "L", "R":
			return 0.5
		case "C":
			return minus3
		case "Ls", "Rs":
			return 0.5
		case "LFE":
			return 0
		}
	case "L":
		switch source {
		case "M":
			return 1
		case "L":
			return 1
		case "C", "Ls":
			return minus3
		case "R", "Rs", "LFE":
			return 0
		}
	case "R":
		switch source {
		case "M":
			return 1
		case "R":
			return 1
		case "C", "Rs":
			return minus3
		case "L", "Ls", "LFE":
			return 0
		}
	}
	return 0
}

// Coefficient exposes one gain for auditing and tests.
func (m *Matrix) Coefficient(dstCh, srcCh int) float64 {
	if dstCh < 0 || dstCh >= len(m.coeff) || srcCh < 0 || srcCh >= len(m.Src) {
		return 0
	}
	return m.coeff[dstCh][srcCh]
}

// Apply maps a block onto the destination layout, preserving timeline
// position, rate and sequence number.
func (m *Matrix) Apply(block media.Block) (media.Block, error) {
	if block.Channels != len(m.Src) {
		return media.Block{}, media.NewError(media.PhaseLayout, media.ExitInput, block.Track, block.Seq, false, "apply",
			fmt.Errorf("block has %d channels, matrix expects %d", block.Channels, len(m.Src)))
	}
	frames := block.Frames()
	out := make([]float64, frames*len(m.Dst.Labels))
	for f := 0; f < frames; f++ {
		for d := range m.Dst.Labels {
			var acc float64
			for s := range m.Src {
				acc += block.Samples[f*block.Channels+s] * m.coeff[d][s]
			}
			out[f*len(m.Dst.Labels)+d] = acc
		}
	}
	return media.Block{
		Track:    block.Track,
		Seq:      block.Seq,
		Start:    block.Start,
		Rate:     block.Rate,
		Channels: len(m.Dst.Labels),
		Samples:  out,
	}, nil
}
