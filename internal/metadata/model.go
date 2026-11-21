// Package metadata parses self-generated MXF and MP4 track descriptions into a
// common canonical form: exact rational timebases, normalized rotation and
// crop, resolved colour description and a seekable keyframe index.
package metadata

import (
	"fmt"
	"sort"
	"strings"

	"mediamerge/internal/clock"
	"mediamerge/internal/fault"
)

// Kind is a media track kind.
type Kind string

const (
	KindVideo Kind = "video"
	KindAudio Kind = "audio"
)

// Crop is a clean-aperture window in top-left pixel coordinates. Width and
// Height are extents, not right/bottom edges.
type Crop struct {
	X      int
	Y      int
	Width  int
	Height int
}

func (c Crop) String() string {
	return fmt.Sprintf("%dx%d+%d+%d", c.Width, c.Height, c.X, c.Y)
}

// Validate checks the window against the coded picture. The returned span is
// the full track range, because a crop defect affects every frame.
func (c Crop) Validate(track *Track) error {
	span := track.Span()
	if c.Width <= 0 || c.Height <= 0 {
		return fault.NewSpan(fault.StageResolve, fault.CodeCropOutOfBounds, track.ID, span,
			fmt.Sprintf("crop %s has non-positive extent", c))
	}
	if c.X < 0 || c.Y < 0 {
		return fault.NewSpan(fault.StageResolve, fault.CodeCropOutOfBounds, track.ID, span,
			fmt.Sprintf("crop %s starts outside picture", c))
	}
	if c.X+c.Width > track.Width || c.Y+c.Height > track.Height {
		return fault.NewSpan(fault.StageResolve, fault.CodeCropOutOfBounds, track.ID, span,
			fmt.Sprintf("crop %s exceeds picture %dx%d", c, track.Width, track.Height))
	}
	return nil
}

// ColorDesc is a partial CICP description. Nil fields are unknown, and code 2
// ("unspecified") is treated as unknown rather than as a description.
type ColorDesc struct {
	Primaries *uint16
	Transfer  *uint16
	Matrix    *uint16
	FullRange *bool
	// Source names where this description came from: container, track or
	// explicit. It is diagnostic only and never part of the canonical hash.
	Source string
}

func (d ColorDesc) Empty() bool {
	return d.Primaries == nil && d.Transfer == nil && d.Matrix == nil && d.FullRange == nil
}

// normaliseValue maps ISO/IEC 23001-8 "unspecified" (2) to absence.
func normU16(v uint16) *uint16 {
	if v == 2 {
		return nil
	}
	c := v
	return &c
}

func normRange(v uint16) *bool {
	switch v {
	case 0:
		f := false
		return &f
	case 1:
		t := true
		return &t
	default:
		return nil
	}
}

// NewColorDesc builds a description, dropping unspecified CICP values.
func NewColorDesc(primaries, transfer, matrix uint16, fullRange *bool, source string) ColorDesc {
	return ColorDesc{
		Primaries: normU16(primaries),
		Transfer:  normU16(transfer),
		Matrix:    normU16(matrix),
		FullRange: fullRange,
		Source:    source,
	}
}

var primariesNames = map[uint16]string{
	1: "bt709", 4: "bt470m", 5: "bt470bg", 6: "smpte170m", 7: "smpte240m",
	8: "film", 9: "bt2020", 10: "smpte428", 11: "smpte431", 12: "smpte432",
}

var transferNames = map[uint16]string{
	1: "bt709", 4: "gamma22", 5: "gamma28", 6: "smpte170m", 7: "smpte240m",
	8: "linear", 9: "log100", 10: "log316", 11: "iec61966-2-4", 12: "bt1361",
	13: "srgb", 14: "bt2020-10", 15: "bt2020-12", 16: "smpte2084",
	17: "smpte428", 18: "arib-std-b67",
}

var matrixNames = map[uint16]string{
	0: "identity", 1: "bt709", 4: "fcc", 5: "bt470bg", 6: "smpte170m",
	7: "smpte240m", 8: "ycgco", 9: "bt2020nc", 10: "bt2020c", 11: "smpte2085",
	12: "chroma-derived-nc", 13: "chroma-derived-c", 14: "ictcp",
}

func u16Name(names map[uint16]string, v uint16) string {
	if n, ok := names[v]; ok {
		return n
	}
	return fmt.Sprintf("cicp-%d", v)
}

func rangeName(v bool) string {
	if v {
		return "full"
	}
	return "limited"
}

// ResolvedColor is a complete colour description after precedence.
type ResolvedColor struct {
	Primaries     uint16
	PrimariesName string
	Transfer      uint16
	TransferName  string
	Matrix        uint16
	MatrixName    string
	FullRange     bool
	// Sources records which level supplied each field.
	Sources map[string]string
}

func (c ResolvedColor) Canonical() string {
	return strings.Join([]string{
		fmt.Sprintf("p=%d:%s", c.Primaries, c.PrimariesName),
		fmt.Sprintf("t=%d:%s", c.Transfer, c.TransferName),
		fmt.Sprintf("m=%d:%s", c.Matrix, c.MatrixName),
		fmt.Sprintf("r=%s", rangeName(c.FullRange)),
	}, ",")
}

// ResolveColor applies container < track < explicit precedence field by
// field. Every conflict and every missing field becomes a structured
// diagnostic; nothing is overwritten silently. A nil explicit desc means the
// delivery spec did not override that field.
func ResolveColor(report *fault.Report, trackID int, container, track, explicit *ColorDesc) (ResolvedColor, error) {
	type field struct {
		name      string
		container *uint16
		track     *uint16
		explicit  *uint16
		names     map[uint16]string
	}
	fields := []field{
		{"primaries", levelPrimaries(container), levelPrimaries(track), levelPrimaries(explicit), primariesNames},
		{"transfer", levelTransfer(container), levelTransfer(track), levelTransfer(explicit), transferNames},
		{"matrix", levelMatrix(container), levelMatrix(track), levelMatrix(explicit), matrixNames},
	}
	var out ResolvedColor
	out.Sources = map[string]string{}
	values := make([]uint16, 0, 3)
	for _, f := range fields {
		var chosen *uint16
		source := ""
		var rejected []string
		for _, cand := range []struct {
			src string
			v   *uint16
		}{{"container", f.container}, {"track", f.track}, {"explicit", f.explicit}} {
			if cand.v == nil {
				continue
			}
			if chosen == nil {
				chosen, source = cand.v, cand.src
				continue
			}
			if *cand.v != *chosen {
				rejected = append(rejected, fmt.Sprintf("%s=%s", source, u16Name(f.names, *chosen)))
			}
			// The later, more specific level replaces the earlier one; the
			// displaced value stays visible in the diagnostic.
			chosen, source = cand.v, cand.src
		}
		if chosen == nil {
			report.Add(fault.Diagnostic{
				Code: fault.CodeColorMissing, Severity: fault.SeverityError,
				Stage: fault.StageResolve, TrackID: trackID, Field: f.name,
				Message: "no level supplies this colour field",
			})
			continue
		}
		if len(rejected) > 0 {
			report.Add(fault.Diagnostic{
				Code: fault.CodeColorConflict, Severity: fault.SeverityWarn,
				Stage: fault.StageResolve, TrackID: trackID, Field: f.name,
				Chosen: source + "=" + u16Name(f.names, *chosen), Rejected: rejected,
				Message: "resolved by container<track<explicit precedence",
			})
		}
		values = append(values, *chosen)
		out.Sources[f.name] = source
	}

	// Range flag precedence, with the same diagnostic rules.
	var chosenRange *bool
	rangeSource := ""
	var rejectedRange []string
	for _, cand := range []struct {
		src string
		v   *bool
	}{{"container", levelRange(container)}, {"track", levelRange(track)}, {"explicit", levelRange(explicit)}} {
		if cand.v == nil {
			continue
		}
		if chosenRange == nil {
			chosenRange, rangeSource = cand.v, cand.src
			continue
		}
		if *cand.v != *chosenRange {
			rejectedRange = append(rejectedRange, fmt.Sprintf("%s=%s", rangeSource, rangeName(*chosenRange)))
		}
		chosenRange, rangeSource = cand.v, cand.src
	}
	if chosenRange == nil {
		report.Add(fault.Diagnostic{
			Code: fault.CodeColorMissing, Severity: fault.SeverityError,
			Stage: fault.StageResolve, TrackID: trackID, Field: "range",
			Message: "no level supplies the quantisation range",
		})
	} else {
		if len(rejectedRange) > 0 {
			report.Add(fault.Diagnostic{
				Code: fault.CodeColorConflict, Severity: fault.SeverityWarn,
				Stage: fault.StageResolve, TrackID: trackID, Field: "range",
				Chosen: rangeSource + "=" + rangeName(*chosenRange), Rejected: rejectedRange,
				Message: "resolved by container<track<explicit precedence",
			})
		}
		out.FullRange = *chosenRange
		out.Sources["range"] = rangeSource
	}

	if err := report.AsError(fault.StageResolve); err != nil {
		return out, err
	}
	out.Primaries, out.PrimariesName = values[0], u16Name(primariesNames, values[0])
	out.Transfer, out.TransferName = values[1], u16Name(transferNames, values[1])
	out.Matrix, out.MatrixName = values[2], u16Name(matrixNames, values[2])
	return out, nil
}

// Field accessors keep precedence per-field rather than per-descriptor: one
// level may supply primaries while another supplies the matrix.
func levelPrimaries(d *ColorDesc) *uint16 {
	if d == nil {
		return nil
	}
	return d.Primaries
}

func levelTransfer(d *ColorDesc) *uint16 {
	if d == nil {
		return nil
	}
	return d.Transfer
}

func levelMatrix(d *ColorDesc) *uint16 {
	if d == nil {
		return nil
	}
	return d.Matrix
}

func levelRange(d *ColorDesc) *bool {
	if d == nil {
		return nil
	}
	return d.FullRange
}

// Keyframe is one seek point. Sample is the 1-based decode-order sample
// number, Tick is the presentation tick in the track timebase.
type Keyframe struct {
	Sample int
	Tick   int64
	At     clock.Rational
}

// KeyframeIndex is a sorted index of decode-order key samples with their
// presentation times.
type KeyframeIndex struct {
	entries []Keyframe
	byTick  bool
}

// BuildKeyframeIndex builds the index from per-sample deltas (in presentation
// order) and 1-based key sample numbers. Deltas must be positive.
func BuildKeyframeIndex(deltas []int64, keySamples []int, tb clock.Timebase) (KeyframeIndex, error) {
	if len(deltas) == 0 {
		return KeyframeIndex{}, fmt.Errorf("metadata: no sample timing")
	}
	ticks := make([]int64, len(deltas))
	var t int64
	for i, d := range deltas {
		if d <= 0 {
			return KeyframeIndex{}, fmt.Errorf("metadata: non-positive sample delta %d at sample %d", d, i+1)
		}
		// Sample 1 presents at tick 0; each following sample starts after
		// the previous sample's delta.
		ticks[i] = t
		t += d
	}
	seen := make(map[int]bool, len(keySamples))
	var entries []Keyframe
	for _, s := range keySamples {
		if s < 1 || s > len(ticks) {
			return KeyframeIndex{}, fmt.Errorf("metadata: key sample %d outside 1..%d", s, len(ticks))
		}
		if seen[s] {
			return KeyframeIndex{}, fmt.Errorf("metadata: duplicate key sample %d", s)
		}
		seen[s] = true
		entries = append(entries, Keyframe{Sample: s, Tick: ticks[s-1], At: tb.Duration(ticks[s-1])})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Tick < entries[j].Tick })
	return KeyframeIndex{entries: entries, byTick: true}, nil
}

func (k KeyframeIndex) Empty() bool { return len(k.entries) == 0 }

func (k KeyframeIndex) Entries() []Keyframe {
	out := make([]Keyframe, len(k.entries))
	copy(out, k.entries)
	return out
}

// Hit returns the last keyframe at or before tick. The boolean is false when
// the target precedes every keyframe.
func (k KeyframeIndex) Hit(tick int64) (Keyframe, bool) {
	i := sort.Search(len(k.entries), func(i int) bool { return k.entries[i].Tick > tick })
	if i == 0 {
		return Keyframe{}, false
	}
	return k.entries[i-1], true
}

// HitSpan reports whether a seek into span.Start lands on a keyframe inside
// the span, and returns it.
func (k KeyframeIndex) HitSpan(span clock.Span, tb clock.Timebase) (Keyframe, bool) {
	if k.Empty() {
		return Keyframe{}, false
	}
	start := ticksFor(span.Start, tb)
	end := ticksFor(span.End, tb)
	kf, ok := k.Hit(start)
	if !ok {
		return Keyframe{}, false
	}
	if kf.Tick >= end {
		return Keyframe{}, false
	}
	return kf, true
}

func ticksFor(at clock.Rational, tb clock.Timebase) int64 {
	// at * tb.Rate, truncated; used only for seek lookup.
	num := int64(at.Num) * tb.Rate.Num
	den := int64(at.Den) * tb.Rate.Den
	if den == 0 {
		return 0
	}
	return num / den
}

// Track is the canonical description shared by parsers and the pipeline.
type Track struct {
	ID            int
	Kind          Kind
	Timebase      clock.Timebase
	DurationTicks int64

	// Video.
	Width, Height int
	Rotation      int
	Crop          *Crop
	Color         ColorDesc
	// Resolved is the delivery colour after precedence; nil for audio and
	// for video tracks that have not been normalized yet.
	Resolved *ResolvedColor

	// Audio.
	SampleRate int
	Channels   int
	Layout     string
	BitDepth   int

	Keyframes KeyframeIndex
}

// Span returns the track's full media range in seconds.
func (t *Track) Span() clock.Span { return t.Timebase.Span(0, t.DurationTicks) }

// NormalizeRotation maps a raw angle to 0/90/180/270. Non-right angles are
// rejected instead of being rounded. Negative angles and full turns are
// folded into the canonical quadrant.
func NormalizeRotation(trackID int, degrees float64, span clock.Span) (int, error) {
	if degrees < 0 {
		degrees = 360 + float64(int(degrees)%360)
		if degrees >= 360 {
			degrees -= 360
		}
	}
	candidates := []int{0, 90, 180, 270}
	for _, c := range candidates {
		diff := degrees - float64(c)
		if diff < 0 {
			diff = -diff
		}
		if diff < 0.001 {
			return c, nil
		}
	}
	for _, c := range candidates {
		diff := degrees - float64(c+360)
		if diff < 0 {
			diff = -diff
		}
		if diff < 0.001 {
			return c, nil
		}
	}
	return 0, fault.NewSpan(fault.StageResolve, fault.CodeRotationInvalid, trackID, span,
		fmt.Sprintf("rotation %.3f is not a multiple of 90 degrees", degrees))
}

// Normalize produces the canonical delivery form: rotation snapped to a right
// angle, crop checked, colour resolved and timing reduced. It appends all
// findings to report and returns the resolved colour.
func (t *Track) Normalize(report *fault.Report, container *ColorDesc, explicit *ColorDesc) (ResolvedColor, error) {
	rot, err := NormalizeRotation(t.ID, float64(t.Rotation), t.Span())
	if err != nil {
		return ResolvedColor{}, err
	}
	t.Rotation = rot
	if t.Crop != nil {
		if err := t.Crop.Validate(t); err != nil {
			return ResolvedColor{}, err
		}
	}
	if t.Kind == KindVideo {
		res, err := ResolveColor(report, t.ID, container, &t.Color, explicit)
		if err != nil {
			return res, err
		}
		t.Resolved = &res
		return res, nil
	}
	return ResolvedColor{}, nil
}

// Canonical returns deterministic text used for stage digests. Field order is
// fixed and no maps are iterated.
func (t *Track) Canonical() string {
	var b strings.Builder
	fmt.Fprintf(&b, "track=%d kind=%s tb=%s dur=%d", t.ID, t.Kind, t.Timebase, t.DurationTicks)
	switch t.Kind {
	case KindVideo:
		fmt.Fprintf(&b, " size=%dx%d rot=%d", t.Width, t.Height, t.Rotation)
		if t.Crop != nil {
			fmt.Fprintf(&b, " crop=%s", t.Crop)
		} else {
			b.WriteString(" crop=none")
		}
		if t.Resolved != nil {
			fmt.Fprintf(&b, " resolved=%s", t.Resolved.Canonical())
		} else {
			fmt.Fprintf(&b, " color=%s", t.Color.Canonical())
		}
	case KindAudio:
		fmt.Fprintf(&b, " rate=%d ch=%d layout=%s depth=%d", t.SampleRate, t.Channels, t.Layout, t.BitDepth)
	}
	fmt.Fprintf(&b, " keys=%d", len(t.Keyframes.entries))
	return b.String()
}

// CanonicalColorDesc is the raw descriptor text for roundtrip assertions.
func (d ColorDesc) Canonical() string {
	part := func(name string, v *uint16, names map[uint16]string) string {
		if v == nil {
			return name + "=-"
		}
		return name + "=" + u16Name(names, *v)
	}
	r := "r=-"
	if d.FullRange != nil {
		r = "r=" + rangeName(*d.FullRange)
	}
	return strings.Join([]string{
		part("p", d.Primaries, primariesNames),
		part("t", d.Transfer, transferNames),
		part("m", d.Matrix, matrixNames),
		r,
	}, ",")
}

// File is the parser output: tracks plus findings.
type File struct {
	Container   string
	Tracks      []*Track
	Color       *ColorDesc
	Diagnostics fault.Report
}

// TrackByID finds a track or returns nil.
func (f *File) TrackByID(id int) *Track {
	for _, t := range f.Tracks {
		if t.ID == id {
			return t
		}
	}
	return nil
}
