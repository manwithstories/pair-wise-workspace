package mp4

import (
	"fmt"

	mp4 "github.com/abema/go-mp4"

	"mediapack/internal/fault"
)

// find returns every node with the given type below nodes.
func find(nodes []*Node, typ mp4.BoxType) []*Node {
	var out []*Node
	for _, n := range nodes {
		if n.Type == typ {
			out = append(out, n)
		}
		out = append(out, find(n.Children, typ)...)
	}
	return out
}

// findOne returns the first node of the given type, or nil.
func findOne(nodes []*Node, typ mp4.BoxType) *Node {
	for _, n := range nodes {
		if n.Type == typ {
			return n
		}
		if got := findOne(n.Children, typ); got != nil {
			return got
		}
	}
	return nil
}

// Color is the colour metadata carried by a sample entry.
type Color struct {
	Primaries uint16
	Transfer  uint16
	Matrix    uint16
	FullRange bool
}

func colorOf(node *Node) *Color {
	if node == nil || node.Payload == nil {
		return nil
	}
	box, ok := node.Payload.(*mp4.Colr)
	if !ok {
		return nil
	}
	return &Color{Primaries: box.ColourPrimaries, Transfer: box.TransferCharacteristics,
		Matrix: box.MatrixCoefficients, FullRange: box.FullRangeFlag}
}

// Track is the comparable summary of one media track.
type Track struct {
	ID           uint32
	Handler      string
	Timescale    uint32
	Duration     uint64
	SampleCount  int
	SampleDeltas []uint32
	Keyframes    []int // 1-based sample numbers; empty means every sample is a sync sample
	Width        uint16
	Height       uint16
	Channels     uint16
	SampleRate   uint32
	Color        *Color
}

// Summary holds every field the round-trip comparison considers.
type Summary struct {
	MajorBrand   string
	MinorVersion uint32
	Compatible   []string
	Timescale    uint32
	Duration     uint64
	Tracks       []Track
}

// Summary extracts key fields, failing when required structure is absent.
func (f *File) Summary(limits Limits) (*Summary, error) {
	ftyp := findOne(f.Nodes, mp4.BoxTypeFtyp())
	if ftyp == nil || ftyp.Payload == nil {
		return nil, fault.New(fault.ClassInput, "mp4", "*", "missing-ftyp", "file has no ftyp box")
	}
	ft, ok := ftyp.Payload.(*mp4.Ftyp)
	if !ok {
		return nil, fault.New(fault.ClassInput, "mp4", "*", "invalid-ftyp", "ftyp payload has unexpected type")
	}
	s := &Summary{MajorBrand: string(ft.MajorBrand[:]), MinorVersion: ft.MinorVersion}
	for _, b := range ft.CompatibleBrands {
		s.Compatible = append(s.Compatible, string(b.CompatibleBrand[:]))
	}

	mvhd := findOne(f.Nodes, mp4.BoxTypeMvhd())
	if mvhd == nil || mvhd.Payload == nil {
		return nil, fault.New(fault.ClassInput, "mp4", "*", "missing-mvhd", "file has no mvhd box")
	}
	m, ok := mvhd.Payload.(*mp4.Mvhd)
	if !ok {
		return nil, fault.New(fault.ClassInput, "mp4", "*", "invalid-mvhd", "mvhd payload has unexpected type")
	}
	s.Timescale = m.Timescale
	s.Duration = m.GetDuration()

	traks := find(f.Nodes, mp4.BoxTypeTrak())
	if len(traks) > limits.MaxTracks {
		return nil, fault.New(fault.ClassLimit, "mp4", "*", "too-many-tracks",
			fmt.Sprintf("%d tracks exceeds limit %d", len(traks), limits.MaxTracks))
	}
	for i, trak := range traks {
		t, err := summarizeTrack(trak, i)
		if err != nil {
			return nil, err
		}
		s.Tracks = append(s.Tracks, t)
	}
	return s, nil
}

func missing(field string, index int) *fault.Error {
	return fault.New(fault.ClassInput, "mp4", "*", "missing-"+field,
		fmt.Sprintf("track %d has no %s", index, field)).WithSegment(index + 1)
}

func summarizeTrack(trak *Node, index int) (Track, error) {
	var t Track
	tkhd := findOne(trak.Children, mp4.BoxTypeTkhd())
	if tkhd == nil || tkhd.Payload == nil {
		return t, missing("tkhd", index)
	}
	if box, ok := tkhd.Payload.(*mp4.Tkhd); ok {
		t.ID = box.TrackID
		t.Width = uint16(box.Width >> 16)
		t.Height = uint16(box.Height >> 16)
	}
	mdhd := findOne(trak.Children, mp4.BoxTypeMdhd())
	if mdhd == nil || mdhd.Payload == nil {
		return t, missing("mdhd", index)
	}
	if box, ok := mdhd.Payload.(*mp4.Mdhd); ok {
		t.Timescale = box.Timescale
		t.Duration = box.GetDuration()
	}
	hdlr := findOne(trak.Children, mp4.BoxTypeHdlr())
	if hdlr == nil || hdlr.Payload == nil {
		return t, missing("hdlr", index)
	}
	if box, ok := hdlr.Payload.(*mp4.Hdlr); ok {
		t.Handler = string(box.HandlerType[:])
	}

	stsz := findOne(trak.Children, mp4.BoxTypeStsz())
	if stsz == nil || stsz.Payload == nil {
		return t, missing("stsz", index)
	}
	if box, ok := stsz.Payload.(*mp4.Stsz); ok {
		t.SampleCount = int(box.SampleCount)
	}
	stts := findOne(trak.Children, mp4.BoxTypeStts())
	if stts == nil || stts.Payload == nil {
		return t, missing("stts", index)
	}
	if box, ok := stts.Payload.(*mp4.Stts); ok {
		for _, e := range box.Entries {
			for j := uint32(0); j < e.SampleCount; j++ {
				t.SampleDeltas = append(t.SampleDeltas, e.SampleDelta)
				if len(t.SampleDeltas) > 1_000_000 {
					return t, fault.New(fault.ClassLimit, "mp4", "*", "too-many-samples",
						"stts expands beyond the sample limit")
				}
			}
		}
	}
	if stss := findOne(trak.Children, mp4.BoxTypeStss()); stss != nil && stss.Payload != nil {
		if box, ok := stss.Payload.(*mp4.Stss); ok {
			for _, n := range box.SampleNumber {
				t.Keyframes = append(t.Keyframes, int(n))
			}
		}
	}

	t.Color = colorOf(findOne(trak.Children, mp4.BoxTypeColr()))

	if stsd := findOne(trak.Children, mp4.BoxTypeStsd()); stsd != nil {
		for _, entry := range stsd.Children {
			switch box := entry.Payload.(type) {
			case *mp4.VisualSampleEntry:
				t.Width = box.Width
				t.Height = box.Height
			case *mp4.AudioSampleEntry:
				t.Channels = box.ChannelCount
				t.SampleRate = box.SampleRate >> 16
			}
			if c := colorOf(findOne(entry.Children, mp4.BoxTypeColr())); c != nil {
				t.Color = c
			}
		}
	}
	return t, nil
}
