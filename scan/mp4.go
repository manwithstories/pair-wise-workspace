package scan

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"mediaslice/media"
)

// boxNode is one node of the lightweight full-tree box walk used to locate
// truncation at exact byte offsets and to record mdat bounds.
type boxNode struct {
	info     mp4.BoxInfo
	parent   *boxNode
	children []*boxNode
}

// walkBoxes performs a recursive header-only scan of the whole file. A box
// whose declared size runs past EOF yields KindTruncatedBox carrying the
// exact byte offset (error_bytes=0), before any payload parsing is attempted.
func walkBoxes(f *os.File, size int64, source string) (*boxNode, media.ErrorList) {
	root := &boxNode{}
	var errs media.ErrorList
	var recurse func(parent *boxNode, start, end int64)
	recurse = func(parent *boxNode, start, end int64) {
		pos := start
		for pos+8 <= end {
			hdr := make([]byte, 8)
			if _, err := f.ReadAt(hdr, pos); err != nil {
				errs = append(errs, media.NewError(media.KindTruncatedBox, source,
					"box header unreadable").WithOffset(pos, 0))
				return
			}
			size32 := binary.BigEndian.Uint32(hdr[:4])
			typ := string(hdr[4:])
			hdrSize := int64(8)
			boxSize := int64(size32)
			if size32 == 1 {
				ext := make([]byte, 8)
				if _, err := f.ReadAt(ext, pos+8); err != nil {
					errs = append(errs, media.NewError(media.KindTruncatedBox, source,
						"64-bit box size unreadable").WithOffset(pos, 0))
					return
				}
				boxSize = int64(binary.BigEndian.Uint64(ext))
				hdrSize = 16
			} else if size32 == 0 {
				boxSize = end - pos // box extends to EOF
			}
			if boxSize < hdrSize || pos+boxSize > end {
				errs = append(errs, media.NewError(media.KindTruncatedBox, source,
					fmt.Sprintf("box %q ends past EOF", typ)).WithOffset(pos, 0))
				return
			}
			node := &boxNode{
				info: mp4.BoxInfo{
					Offset:     uint64(pos),
					Size:       uint64(boxSize),
					HeaderSize: uint64(hdrSize),
					Type:       mp4.StrToBoxType(typ),
				},
				parent: parent,
			}
			parent.children = append(parent.children, node)
			if isContainer(typ) {
				recurse(node, pos+hdrSize, pos+boxSize)
			}
			pos += boxSize
		}
		if pos != end {
			errs = append(errs, media.NewError(media.KindTruncatedBox, source,
				"trailing partial box header").WithOffset(pos, 0))
		}
	}
	recurse(root, 0, size)
	return root, errs
}

// isContainer reports which box types nest child boxes.
func isContainer(typ string) bool {
	switch typ {
	case "moov", "trak", "mdia", "minf", "stbl", "edts", "udta", "meta",
		"mvex", "moof", "traf":
		return true
	}
	return false
}

// findFirst returns the first node of the requested box type under n in
// depth-first order.
func (n *boxNode) findFirst(typ string) *boxNode {
	for _, c := range n.children {
		if c.info.Type.String() == typ {
			return c
		}
		if got := c.findFirst(typ); got != nil {
			return got
		}
	}
	return nil
}

func (n *boxNode) findAll(typ string) []*boxNode {
	var out []*boxNode
	for _, c := range n.children {
		if c.info.Type.String() == typ {
			out = append(out, c)
		}
		out = append(out, c.findAll(typ)...)
	}
	return out
}

// PayloadAt seeks to a box payload and unmarshals it into dst using go-mp4.
func payloadAt(f *os.File, node *boxNode, dst mp4.IBox) error {
	if _, err := f.Seek(int64(node.info.Offset)+int64(node.info.HeaderSize), io.SeekStart); err != nil {
		return err
	}
	payloadSize := node.info.Size - node.info.HeaderSize
	_, err := mp4.Unmarshal(f, payloadSize, dst, mp4.Context{})
	return err
}
