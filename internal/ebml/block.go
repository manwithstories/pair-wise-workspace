package ebml

import (
	"bytes"
	"encoding/binary"
	"io"

	"mkvarchive/internal/model"
)

// Block is one Matroska SimpleBlock or Block (inside BlockGroup). The full
// payload bytes are retained verbatim so an archival copy never rewrites a
// compressed frame; parsed header fields expose track/timecode semantics.
type Block struct {
	TrackNumber      uint64
	RelativeTimecode int16
	Flags            byte
	Keyframe         bool
	ElementID        uint32
	ElementHeaderOff int64
	PayloadOff       int64
	Size             int64
	Payload          []byte
	ClusterTimecode  int64
}

// AbsoluteTimecode returns cluster timecode plus the block-relative offset,
// in raw Matroska timecode units (convert with the segment TimestampScale).
func (b *Block) AbsoluteTimecode() int64 {
	return b.ClusterTimecode + int64(b.RelativeTimecode)
}

// MatroskaTrackNumber converts the on-wire VINT data value (track number
// minus one) to the declared Matroska track number.
func (b *Block) MatroskaTrackNumber() uint64 {
	return b.TrackNumber + 1
}

// buildBlock parses a block element payload into a Block, anchoring any
// malformed-header error at the element header offset.
func buildBlock(e *Element, data []byte, clusterTC int64, id uint32) (*Block, error) {
	v, err := ReadVINT(bytes.NewReader(data), e.DataOff)
	if err != nil {
		return nil, err
	}
	headerLen := v.Width + 3
	if len(data) < headerLen {
		return nil, model.NewError("EBML_BLOCK_TRUNCATED",
			"block header extends past its element payload", e.HeaderOff)
	}
	tc := int16(binary.BigEndian.Uint16(data[v.Width : v.Width+2]))
	flags := data[v.Width+2]
	return &Block{
		TrackNumber:      v.Value,
		RelativeTimecode: tc,
		Flags:            flags,
		Keyframe:         id == model.IDSimpleBlock && flags&0x80 != 0,
		ElementID:        id,
		ElementHeaderOff: e.HeaderOff,
		PayloadOff:       e.DataOff,
		Size:             e.Size,
		Payload:          append([]byte(nil), data...),
		ClusterTimecode:  clusterTC,
	}, nil
}

// readBlockGroup walks a BlockGroup master and returns its Block child.
func (s *Scanner) readBlockGroup(group *Element) (*Block, error) {
	cr, closer, err := s.r.ChildReader(group, s.clusterEnd)
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	for {
		child, err := cr.Next()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if child.ID == model.IDBlock {
			data, err := cr.ReadData(child)
			if err != nil {
				return nil, err
			}
			return buildBlock(child, data, s.clusterTC, model.IDBlock)
		}
		if err = cr.Skip(child); err != nil {
			return nil, err
		}
	}
}
