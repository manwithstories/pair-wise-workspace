package normalize

import "fmt"

// ColorMeta is a colour description attached to a segment.
type ColorMeta struct {
	Primaries string
	Transfer  string
	Matrix    string
}

// IsZero reports whether the metadata is absent.
func (m ColorMeta) IsZero() bool {
	return m.Primaries == "" && m.Transfer == "" && m.Matrix == ""
}

// ColorSource identifies where colour metadata came from. Higher priority
// sources win; the order is explicit and recorded in every decision.
type ColorSource int

const (
	SourceContainer ColorSource = iota // highest priority
	SourceBitstream
	SourceDefault // lowest priority
)

func (s ColorSource) String() string {
	switch s {
	case SourceContainer:
		return "container"
	case SourceBitstream:
		return "bitstream"
	default:
		return "default"
	}
}

// DefaultColor is applied when no source carries metadata.
var DefaultColor = ColorMeta{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}

// ColorDecision records the resolution for one segment.
type ColorDecision struct {
	Segment   int
	Chosen    ColorMeta
	Source    ColorSource
	Conflict  bool
	Container ColorMeta
	Bitstream ColorMeta
}

// ResolveColor picks the effective colour metadata by explicit priority
// container > bitstream > default. Conflicts are reported, never silently
// merged, and no pixel conversion is performed or implied.
func ResolveColor(segment int, container, bitstream ColorMeta) ColorDecision {
	d := ColorDecision{Segment: segment, Container: container, Bitstream: bitstream}
	switch {
	case !container.IsZero():
		d.Chosen, d.Source = container, SourceContainer
		d.Conflict = !bitstream.IsZero() && bitstream != container
	case !bitstream.IsZero():
		d.Chosen, d.Source = bitstream, SourceBitstream
	default:
		d.Chosen, d.Source = DefaultColor, SourceDefault
	}
	return d
}

// String renders the decision for the report log.
func (d ColorDecision) String() string {
	s := fmt.Sprintf("segment=%d color=%s/%s/%s source=%s",
		d.Segment, d.Chosen.Primaries, d.Chosen.Transfer, d.Chosen.Matrix, d.Source)
	if d.Conflict {
		s += fmt.Sprintf(" conflict(container=%s/%s/%s bitstream=%s/%s/%s)",
			d.Container.Primaries, d.Container.Transfer, d.Container.Matrix,
			d.Bitstream.Primaries, d.Bitstream.Transfer, d.Bitstream.Matrix)
	}
	return s
}
