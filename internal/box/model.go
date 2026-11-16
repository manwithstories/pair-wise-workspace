// Package box implements a self-contained MP4 (ISO BMFF) box tree parser.
// go-mp4 is used only as the low-level box-header codec; the tree walk,
// container handling, uuid boxes and bounds checking live here.
package box

// Node is one box in the parsed tree. Leaf boxes keep only their header
// geometry; payload bytes are read on demand via an io.ReaderAt.
type Node struct {
	Type       string    // four-character code, or "uuid"
	Offset     int64     // absolute file offset of the box header
	Size       int64     // total size in bytes, header included
	HeaderSize int64     // 8, 16, or 24/32 for uuid boxes
	UUID       *[16]byte // usertype, set only when Type == "uuid"
	Children   []*Node
}

// DataOffset returns the absolute offset of the box payload.
func (n *Node) DataOffset() int64 { return n.Offset + n.HeaderSize }

// DataSize returns the payload size in bytes.
func (n *Node) DataSize() int64 { return n.Size - n.HeaderSize }

// End returns the absolute offset one past the box.
func (n *Node) End() int64 { return n.Offset + n.Size }

// Find returns the first direct child of the given type, or nil.
func (n *Node) Find(typ string) *Node {
	for _, c := range n.Children {
		if c.Type == typ {
			return c
		}
	}
	return nil
}

// FindAll returns every direct child of the given type.
func (n *Node) FindAll(typ string) []*Node {
	var out []*Node
	for _, c := range n.Children {
		if c.Type == typ {
			out = append(out, c)
		}
	}
	return out
}

// FindPath walks a chain of child types, e.g. FindPath("moov","trak","mdia").
// The first element may match the receiver itself.
func (n *Node) FindPath(types ...string) *Node {
	cur := n
	for i, t := range types {
		if i == 0 && cur.Type == t {
			continue
		}
		cur = cur.Find(t)
		if cur == nil {
			return nil
		}
	}
	return cur
}

// Count returns the total number of nodes in the subtree, receiver included.
func (n *Node) Count() int {
	c := 1
	for _, ch := range n.Children {
		c += ch.Count()
	}
	return c
}

// Forest is a parsed file: the ordered list of top-level boxes.
type Forest struct {
	Roots []*Node
	Size  int64 // file size in bytes
}

// Find returns the first top-level box of the given type, or nil.
func (f *Forest) Find(typ string) *Node {
	for _, r := range f.Roots {
		if r.Type == typ {
			return r
		}
	}
	return nil
}

// Count returns the total number of boxes in the file.
func (f *Forest) Count() int {
	c := 0
	for _, r := range f.Roots {
		c += r.Count()
	}
	return c
}
