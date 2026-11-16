package box

import (
	"errors"
	"fmt"
	"io"

	mp4 "github.com/abema/go-mp4"
)

// StructureError reports a malformed file. Offset is the file position of
// the offending box (or the position where reading failed).
type StructureError struct {
	Offset int64
	Reason string
}

func (e *StructureError) Error() string {
	return fmt.Sprintf("mp4 structure error at offset %d: %s", e.Offset, e.Reason)
}

// IsStructure reports whether err is (or wraps) a StructureError.
func IsStructure(err error) bool {
	var se *StructureError
	return errors.As(err, &se)
}

// Limits bound parser resource use. Zero fields take defaults.
type Limits struct {
	MaxBoxes  int   // default 1<<20
	MaxDepth  int   // default 32
	MaxFile   int64 // default 1<<40 (1 TiB)
}

func (l *Limits) withDefaults() Limits {
	out := Limits{MaxBoxes: 1 << 20, MaxDepth: 32, MaxFile: 1 << 40}
	if l != nil {
		out = *l
		if out.MaxBoxes <= 0 {
			out.MaxBoxes = 1 << 20
		}
		if out.MaxDepth <= 0 {
			out.MaxDepth = 32
		}
		if out.MaxFile <= 0 {
			out.MaxFile = 1 << 40
		}
	}
	return out
}

// containerTypes are boxes whose payload is a sequence of child boxes.
// "meta" is deliberately excluded: its payload starts with a 4-byte
// full-box header before children, and fixtures never use it.
var containerTypes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true,
	"dinf": true, "stbl": true, "edts": true, "udta": true,
	"moof": true, "traf": true,
}

// ParseFile opens path and parses its box tree.
func ParseFile(path string, lim *Limits) (*Forest, error) {
	f, size, err := OpenBounded(path, lim.withDefaults().MaxFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f, size, lim)
}

// Parse reads the full box tree from r, which must expose exactly size
// bytes. Unknown box types are preserved as leaf nodes.
func Parse(r io.ReaderAt, size int64, lim *Limits) (*Forest, error) {
	l := lim.withDefaults()
	if size < 0 || size > l.MaxFile {
		return nil, &StructureError{Offset: 0, Reason: fmt.Sprintf("file size %d out of bounds", size)}
	}
	p := &parser{ra: r, lim: l}
	roots, err := p.parseLevel(0, size, 0)
	if err != nil {
		return nil, err
	}
	return &Forest{Roots: roots, Size: size}, nil
}

type parser struct {
	ra     io.ReaderAt
	lim    Limits
	nboxes int
}

// parseLevel parses the box sequence in [start, end).
func (p *parser) parseLevel(start, end int64, depth int) ([]*Node, error) {
	if depth > p.lim.MaxDepth {
		return nil, &StructureError{Offset: start, Reason: "container nesting too deep"}
	}
	var nodes []*Node
	pos := start
	for pos < end {
		p.nboxes++
		if p.nboxes > p.lim.MaxBoxes {
			return nil, &StructureError{Offset: pos, Reason: "box count limit exceeded"}
		}
		n, err := p.parseOne(pos, end, depth)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
		pos = n.End()
	}
	return nodes, nil
}

func (p *parser) parseOne(off, parentEnd int64, depth int) (*Node, error) {
	if parentEnd-off < 8 {
		return nil, &StructureError{Offset: off, Reason: "truncated box header"}
	}
	// go-mp4 decodes the 8/16-byte common header for us.
	sr := io.NewSectionReader(p.ra, off, parentEnd-off)
	bi, err := mp4.ReadBoxInfo(sr)
	if err != nil {
		return nil, &StructureError{Offset: off, Reason: "truncated box header: " + err.Error()}
	}
	if bi.ExtendToEOF && depth > 0 {
		return nil, &StructureError{Offset: off, Reason: "size-0 box only allowed at top level"}
	}
	n := &Node{
		Type:       bi.Type.String(),
		Offset:     off,
		Size:       int64(bi.Size),
		HeaderSize: int64(bi.HeaderSize),
	}
	if bi.Size > uint64(parentEnd-off) { // also catches int64 overflow of off+size
		return nil, &StructureError{Offset: off, Reason: fmt.Sprintf("box size %d overruns parent bounds", bi.Size)}
	}
	if n.Size < n.HeaderSize {
		return nil, &StructureError{Offset: off, Reason: "box size smaller than header"}
	}
	if n.Type == "uuid" {
		var usertype [16]byte
		if _, err := p.ra.ReadAt(usertype[:], off+n.HeaderSize); err != nil {
			return nil, &StructureError{Offset: off, Reason: "truncated uuid usertype"}
		}
		n.UUID = &usertype
		n.HeaderSize += 16
		if n.Size < n.HeaderSize {
			return nil, &StructureError{Offset: off, Reason: "uuid box smaller than extended header"}
		}
	}
	if containerTypes[n.Type] && n.DataSize() > 0 {
		children, err := p.parseLevel(n.DataOffset(), n.End(), depth+1)
		if err != nil {
			return nil, err
		}
		n.Children = children
	}
	return n, nil
}
