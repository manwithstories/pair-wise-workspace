package mp4

import (
	"encoding/binary"
	"fmt"

	mp4 "github.com/abema/go-mp4"

	"mediapack/internal/fault"
)

// Limits bounds work on untrusted files.
type Limits struct {
	MaxBytes   int64
	MaxTracks  int
	MaxBoxes   int
	MaxBoxDeep int
}

// DefaultLimits is small on purpose; mediapack inspects local fixtures.
func DefaultLimits() Limits {
	return Limits{MaxBytes: 64 << 20, MaxTracks: 8, MaxBoxes: 4096, MaxBoxDeep: 16}
}

// Node is one box with either a typed payload or opaque bytes.
type Node struct {
	Type       mp4.BoxType
	Size       uint64
	HeaderSize uint64
	ExtendEOF  bool
	Payload    mp4.IBox
	Raw        []byte
	Children   []*Node
}

// File is a fully validated box tree.
type File struct {
	Nodes   []*Node
	Unknown []string
	Boxes   int
}

// containers lists box types whose payload is followed by child boxes.
var containers = map[mp4.BoxType]bool{
	mp4.BoxTypeMoov(): true, mp4.BoxTypeTrak(): true, mp4.BoxTypeMdia(): true,
	mp4.BoxTypeMinf(): true, mp4.BoxTypeStbl(): true, mp4.BoxTypeEdts(): true,
	mp4.BoxTypeMvex(): true, mp4.BoxTypeMoof(): true, mp4.BoxTypeTraf(): true,
	mp4.BoxTypeUdta(): true, mp4.BoxTypeDinf(): true,
}

// opaque lists large payload boxes kept as raw bytes: decoding them would
// copy megabytes that the summary never inspects.
var opaque = map[mp4.BoxType]bool{
	mp4.BoxTypeMdat(): true, mp4.BoxTypeFree(): true, mp4.BoxTypeSkip(): true,
}

// prefixSize returns how many payload bytes precede children in a box, and
// whether the box has children at all.
func prefixSize(t mp4.BoxType) (uint64, bool) {
	if containers[t] {
		return 0, true
	}
	switch t {
	case mp4.BoxTypeStsd():
		return 8, true // FullBox + EntryCount
	case mp4.BoxTypeAvc1(), mp4.BoxTypeHvc1(), mp4.BoxTypeMp4v(), mp4.BoxTypeEncv(), mp4.BoxTypeHev1():
		return 78, true // VisualSampleEntry fields
	case mp4.BoxTypeMp4a(), mp4.BoxTypeEnca():
		return 28, true // AudioSampleEntry fields
	}
	return 0, false
}

// Parse validates and decodes data.
func Parse(data []byte, limits Limits) (*File, error) {
	if int64(len(data)) > limits.MaxBytes {
		return nil, fault.New(fault.ClassLimit, "mp4", "*", "file-too-large",
			fmt.Sprintf("%d bytes exceeds limit %d", len(data), limits.MaxBytes))
	}
	f := &File{}
	nodes, err := walk(data, 0, uint64(len(data)), 0, limits, f)
	if err != nil {
		return nil, err
	}
	f.Nodes = nodes
	return f, nil
}

func walk(data []byte, off, end uint64, depth int, limits Limits, f *File) ([]*Node, error) {
	if depth > limits.MaxBoxDeep {
		return nil, fault.New(fault.ClassLimit, "mp4", "*", "too-deep",
			fmt.Sprintf("box nesting exceeds limit %d", limits.MaxBoxDeep))
	}
	var nodes []*Node
	for off < end {
		remaining := end - off
		if remaining < 8 {
			return nil, fault.New(fault.ClassInput, "mp4", "*", "truncated-box",
				fmt.Sprintf("%d trailing bytes at offset %d cannot form a box header", remaining, off))
		}
		size := uint64(binary.BigEndian.Uint32(data[off:]))
		typ := mp4.Uint32ToBoxType(binary.BigEndian.Uint32(data[off+4:]))
		header := uint64(8)
		extendEOF := false
		switch size {
		case 0:
			size = remaining
			extendEOF = true
		case 1:
			if remaining < 16 {
				return nil, fault.New(fault.ClassInput, "mp4", "*", "truncated-box",
					fmt.Sprintf("64-bit header for %s at offset %d is truncated", typ, off))
			}
			size = binary.BigEndian.Uint64(data[off+8:])
			header = 16
		}
		if size < header {
			return nil, fault.New(fault.ClassInput, "mp4", "*", "illegal-box-size",
				fmt.Sprintf("%s at offset %d declares size %d below header size %d", typ, off, size, header))
		}
		if size > remaining {
			return nil, fault.New(fault.ClassInput, "mp4", "*", "truncated-box",
				fmt.Sprintf("%s at offset %d declares size %d but only %d bytes remain", typ, off, size, remaining))
		}
		f.Boxes++
		if f.Boxes > limits.MaxBoxes {
			return nil, fault.New(fault.ClassLimit, "mp4", "*", "too-many-boxes",
				fmt.Sprintf("box count exceeds limit %d", limits.MaxBoxes))
		}

		payloadStart := off + header
		payloadEnd := off + size
		node := &Node{Type: typ, Size: size, HeaderSize: header, ExtendEOF: extendEOF}

		if prefix, hasChildren := prefixSize(typ); hasChildren {
			if boundary := payloadStart + prefix; boundary > payloadEnd {
				return nil, fault.New(fault.ClassInput, "mp4", "*", "truncated-box",
					fmt.Sprintf("%s at offset %d is too short for its fixed fields", typ, off))
			}
			box, _, err := mp4.UnmarshalAny(
				newReader(data[payloadStart:payloadStart+prefix]), typ, prefix, mp4.Context{})
			if err != nil {
				return nil, fault.Wrap(fault.ClassInput, "mp4", "*", "invalid-box",
					fmt.Sprintf("%s at offset %d has invalid fixed fields", typ, off), err)
			}
			node.Payload = box
			children, err := walk(data, payloadStart+prefix, payloadEnd, depth+1, limits, f)
			if err != nil {
				return nil, err
			}
			node.Children = children
		} else if opaque[typ] {
			node.Raw = data[payloadStart:payloadEnd]
		} else if typ.IsSupported(mp4.Context{}) {
			box, _, err := mp4.UnmarshalAny(newReader(data[payloadStart:payloadEnd]), typ,
				payloadEnd-payloadStart, mp4.Context{})
			if err != nil {
				return nil, fault.New(fault.ClassInput, "mp4", "*", "invalid-box",
					fmt.Sprintf("%s at offset %d could not be decoded: %v", typ, off, err))
			}
			node.Payload = box
		} else {
			node.Raw = data[payloadStart:payloadEnd]
			f.Unknown = append(f.Unknown, typ.String())
		}

		nodes = append(nodes, node)
		off += size
	}
	return nodes, nil
}
