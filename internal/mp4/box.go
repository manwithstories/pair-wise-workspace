// Package mp4 parses the fragmented MP4 boxes needed to rebuild segment
// timelines: mvhd, trex, mfhd, tfhd, tfdt and trun.
package mp4

import (
	"encoding/binary"
	"fmt"
)

// Box is a parsed ISO BMFF box. Payload excludes the box header.
type Box struct {
	Type    string
	Size    uint64
	Offset  int64
	Payload []byte
}

// ParseBoxes splits data into a sequence of top-level boxes.
func ParseBoxes(data []byte) ([]*Box, error) {
	var boxes []*Box
	off := 0
	for off < len(data) {
		if len(data)-off < 8 {
			return nil, fmt.Errorf("mp4: truncated box header at offset %d", off)
		}
		size := uint64(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		header := uint64(8)
		switch size {
		case 1:
			if len(data)-off < 16 {
				return nil, fmt.Errorf("mp4: truncated largesize at offset %d", off)
			}
			size = binary.BigEndian.Uint64(data[off+8:])
			header = 16
		case 0:
			size = uint64(len(data) - off)
		}
		if size < header || size > uint64(len(data)-off) {
			return nil, fmt.Errorf("mp4: invalid size %d for box %q at offset %d", size, typ, off)
		}
		boxes = append(boxes, &Box{
			Type:    typ,
			Size:    size,
			Offset:  int64(off),
			Payload: data[off+int(header) : off+int(size)],
		})
		off += int(size)
	}
	return boxes, nil
}

// FindBox returns the first box of the given type, or nil.
func FindBox(boxes []*Box, typ string) *Box {
	for _, b := range boxes {
		if b.Type == typ {
			return b
		}
	}
	return nil
}

// Children parses the payload of a container box into child boxes.
func Children(b *Box) ([]*Box, error) {
	return ParseBoxes(b.Payload)
}

// cursor is a bounds-checked big-endian reader over a box payload.
type cursor struct {
	data []byte
	off  int
}

func (c *cursor) u8() (uint8, error) {
	if c.off+1 > len(c.data) {
		return 0, fmt.Errorf("mp4: unexpected end of box")
	}
	v := c.data[c.off]
	c.off++
	return v, nil
}

func (c *cursor) u24() (uint32, error) {
	if c.off+3 > len(c.data) {
		return 0, fmt.Errorf("mp4: unexpected end of box")
	}
	v := uint32(c.data[c.off])<<16 | uint32(c.data[c.off+1])<<8 | uint32(c.data[c.off+2])
	c.off += 3
	return v, nil
}

func (c *cursor) u32() (uint32, error) {
	if c.off+4 > len(c.data) {
		return 0, fmt.Errorf("mp4: unexpected end of box")
	}
	v := binary.BigEndian.Uint32(c.data[c.off:])
	c.off += 4
	return v, nil
}

func (c *cursor) u64() (uint64, error) {
	if c.off+8 > len(c.data) {
		return 0, fmt.Errorf("mp4: unexpected end of box")
	}
	v := binary.BigEndian.Uint64(c.data[c.off:])
	c.off += 8
	return v, nil
}

func (c *cursor) skip(n int) error {
	if c.off+n > len(c.data) {
		return fmt.Errorf("mp4: unexpected end of box")
	}
	c.off += n
	return nil
}

// fullBox reads the version byte and 24-bit flags of a full box.
func (c *cursor) fullBox() (uint8, uint32, error) {
	v, err := c.u8()
	if err != nil {
		return 0, 0, err
	}
	f, err := c.u24()
	if err != nil {
		return 0, 0, err
	}
	return v, f, nil
}

// InitInfo carries the init-segment facts the scanner needs.
type InitInfo struct {
	Timescale                 uint32
	Duration                  uint64
	TrexDefaultSampleDuration uint32
}

// ParseInit reads timescale and duration from mvhd and the default sample
// duration from trex inside an init segment.
func ParseInit(data []byte) (*InitInfo, error) {
	top, err := ParseBoxes(data)
	if err != nil {
		return nil, err
	}
	moov := FindBox(top, "moov")
	if moov == nil {
		return nil, fmt.Errorf("mp4: init segment has no moov box")
	}
	kids, err := Children(moov)
	if err != nil {
		return nil, err
	}
	info := &InitInfo{}
	mvhd := FindBox(kids, "mvhd")
	if mvhd == nil {
		return nil, fmt.Errorf("mp4: init segment has no mvhd box")
	}
	c := &cursor{data: mvhd.Payload}
	version, _, err := c.fullBox()
	if err != nil {
		return nil, err
	}
	if version == 1 {
		if err := c.skip(16); err != nil {
			return nil, err
		}
	} else {
		if err := c.skip(8); err != nil {
			return nil, err
		}
	}
	if info.Timescale, err = c.u32(); err != nil {
		return nil, err
	}
	if version == 1 {
		if info.Duration, err = c.u64(); err != nil {
			return nil, err
		}
	} else {
		d, err := c.u32()
		if err != nil {
			return nil, err
		}
		info.Duration = uint64(d)
	}
	if mvex := FindBox(kids, "mvex"); mvex != nil {
		trexes, err := Children(mvex)
		if err != nil {
			return nil, err
		}
		if trex := FindBox(trexes, "trex"); trex != nil {
			c := &cursor{data: trex.Payload}
			if _, _, err := c.fullBox(); err != nil {
				return nil, err
			}
			if err := c.skip(8); err != nil { // track_ID + default_sample_description_index
				return nil, err
			}
			if info.TrexDefaultSampleDuration, err = c.u32(); err != nil {
				return nil, err
			}
		}
	}
	return info, nil
}

// SegmentInfo carries the per-fragment facts the scanner needs.
type SegmentInfo struct {
	SequenceNumber    uint32
	HasSequence       bool
	HasTfdt           bool
	BaseDecodeTime    uint64
	SampleCount       uint32
	TrunDuration      uint64
	HasTrunDuration   bool
	TfhdDefaultSample uint32
	HasTfhdDefault    bool
}

// ParseSegment reads mfhd, tfhd, tfdt and trun from a media segment.
func ParseSegment(data []byte) (*SegmentInfo, error) {
	top, err := ParseBoxes(data)
	if err != nil {
		return nil, err
	}
	moof := FindBox(top, "moof")
	if moof == nil {
		return nil, fmt.Errorf("mp4: media segment has no moof box")
	}
	kids, err := Children(moof)
	if err != nil {
		return nil, err
	}
	info := &SegmentInfo{}
	if mfhd := FindBox(kids, "mfhd"); mfhd != nil {
		c := &cursor{data: mfhd.Payload}
		if _, _, err := c.fullBox(); err != nil {
			return nil, err
		}
		if info.SequenceNumber, err = c.u32(); err != nil {
			return nil, err
		}
		info.HasSequence = true
	}
	traf := FindBox(kids, "traf")
	if traf == nil {
		return nil, fmt.Errorf("mp4: media segment has no traf box")
	}
	trafs, err := Children(traf)
	if err != nil {
		return nil, err
	}
	if tfhd := FindBox(trafs, "tfhd"); tfhd != nil {
		if err := parseTfhd(tfhd.Payload, info); err != nil {
			return nil, err
		}
	}
	if tfdt := FindBox(trafs, "tfdt"); tfdt != nil {
		c := &cursor{data: tfdt.Payload}
		version, _, err := c.fullBox()
		if err != nil {
			return nil, err
		}
		if version == 1 {
			if info.BaseDecodeTime, err = c.u64(); err != nil {
				return nil, err
			}
		} else {
			v, err := c.u32()
			if err != nil {
				return nil, err
			}
			info.BaseDecodeTime = uint64(v)
		}
		info.HasTfdt = true
	}
	if trun := FindBox(trafs, "trun"); trun != nil {
		if err := parseTrun(trun.Payload, info); err != nil {
			return nil, err
		}
	}
	return info, nil
}

func parseTfhd(payload []byte, info *SegmentInfo) error {
	c := &cursor{data: payload}
	_, flags, err := c.fullBox()
	if err != nil {
		return err
	}
	if _, err := c.u32(); err != nil { // track_ID
		return err
	}
	if flags&0x000001 != 0 {
		if err := c.skip(8); err != nil { // base_data_offset
			return err
		}
	}
	if flags&0x000002 != 0 {
		if err := c.skip(4); err != nil { // sample_description_index
			return err
		}
	}
	if flags&0x000008 != 0 {
		if info.TfhdDefaultSample, err = c.u32(); err != nil {
			return err
		}
		info.HasTfhdDefault = true
	}
	if flags&0x000010 != 0 {
		if err := c.skip(4); err != nil { // default_sample_size
			return err
		}
	}
	if flags&0x000020 != 0 {
		if err := c.skip(4); err != nil { // default_sample_flags
			return err
		}
	}
	return nil
}

func parseTrun(payload []byte, info *SegmentInfo) error {
	c := &cursor{data: payload}
	_, flags, err := c.fullBox()
	if err != nil {
		return err
	}
	if info.SampleCount, err = c.u32(); err != nil {
		return err
	}
	if flags&0x000001 != 0 {
		if err := c.skip(4); err != nil { // data_offset
			return err
		}
	}
	if flags&0x000004 != 0 {
		if err := c.skip(4); err != nil { // first_sample_flags
			return err
		}
	}
	for i := uint32(0); i < info.SampleCount; i++ {
		if flags&0x000100 != 0 {
			d, err := c.u32()
			if err != nil {
				return err
			}
			info.TrunDuration += uint64(d)
			info.HasTrunDuration = true
		}
		if flags&0x000200 != 0 {
			if err := c.skip(4); err != nil { // sample_size
				return err
			}
		}
		if flags&0x000400 != 0 {
			if err := c.skip(4); err != nil { // sample_flags
				return err
			}
		}
		if flags&0x000800 != 0 {
			if err := c.skip(4); err != nil { // sample_composition_time_offset
				return err
			}
		}
	}
	return nil
}
