package format

import (
	"bytes"
	"encoding/binary"

	"medialab/internal/model"
)

// MP4BoxHeader is a box's absolute location and decoded size.
type MP4BoxHeader struct {
	Type       string
	Start      int64
	HeaderSize int
	Content    int64
	End        int64
}

var mp4Containers = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true,
	"stbl": true, "edts": true, "dinf": true, "udta": true,
}

// ParseMP4 parses the implemented MP4 metadata subset. A bad box length is a
// Fault carrying the box's original absolute offset.
func ParseMP4(data []byte) (*Container, error) {
	if len(data) == 0 {
		return nil, model.New(model.StageParseMP4, "empty_input", 0, 0, errTruncated)
	}
	if len(data) > MaxContainerBytes {
		return nil, model.New(model.StageParseMP4, "input_too_large", 0, 0, errTooLarge)
	}
	c := &Container{Format: "mp4", Raw: data}
	if err := parseMP4Children(data, 0, int64(len(data)), c, nil); err != nil {
		return nil, err
	}
	if len(c.Tracks) == 0 {
		return nil, model.New(model.StageParseMP4, "no_tracks", 0, 0, errUnsupported)
	}
	return c, nil
}

func readMP4Header(data []byte, start, limit int64) (MP4BoxHeader, error) {
	h := MP4BoxHeader{Start: start}
	if start < 0 || start+8 > limit || int(start)+8 > len(data) {
		return h, model.New(model.StageParseMP4, "truncated_box_header", 0, start, errTruncated)
	}
	size32 := binary.BigEndian.Uint32(data[start : start+4])
	h.Type = string(data[start+4 : start+8])
	h.HeaderSize = 8
	size := int64(size32)
	if size32 == 1 {
		if start+16 > limit || int(start)+16 > len(data) {
			return h, model.New(model.StageParseMP4, "truncated_box_header64", 0, start, errTruncated)
		}
		size = int64(binary.BigEndian.Uint64(data[start+8 : start+16]))
		h.HeaderSize = 16
	} else if size32 == 0 {
		size = limit - start
	}
	if size < int64(h.HeaderSize) || start+size > limit || start+size < start {
		return h, model.New(model.StageParseMP4, "box_length_out_of_bounds", 0, start, errBadBoxLength)
	}
	h.Content = start + int64(h.HeaderSize)
	h.End = start + size
	return h, nil
}

func parseMP4Children(data []byte, start, end int64, c *Container, track *Track) error {
	pos := start
	for pos < end {
		h, err := readMP4Header(data, pos, end)
		if err != nil {
			return err
		}
		next := h.End
		childTrack := track
		if h.Type == "trak" {
			childTrack = &Track{ID: uint32(len(c.Tracks) + 1), DataOffset: h.Start}
			c.Tracks = append(c.Tracks, childTrack)
		}
		if mp4Containers[h.Type] {
			if err := parseMP4Children(data, h.Content, h.End, c, childTrack); err != nil {
				return err
			}
		} else if err := parseMP4Leaf(data, h, c, childTrack); err != nil {
			return err
		}
		pos = next
	}
	return nil
}

// BuildMP4 is a small deterministic encoder used by fixtures and roundtrip
// tests. Boxes are supplied as type/content pairs.
func BuildMP4(boxes ...MP4Box) []byte {
	var out bytes.Buffer
	for _, b := range boxes {
		out.Write(b.Encode())
	}
	return out.Bytes()
}

// MP4Box is an in-memory box used by the fixture generator.
type MP4Box struct {
	Type    string
	Content []byte
}

// Encode encodes a normal 32-bit-size box.
func (b MP4Box) Encode() []byte {
	size := uint32(8 + len(b.Content))
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, size)
	buf.WriteString(b.Type)
	buf.Write(b.Content)
	return buf.Bytes()
}

// MP4Container builds a box containing child boxes.
func MP4Container(typ string, children ...MP4Box) MP4Box {
	var content bytes.Buffer
	for _, child := range children {
		content.Write(child.Encode())
	}
	return MP4Box{Type: typ, Content: content.Bytes()}
}
