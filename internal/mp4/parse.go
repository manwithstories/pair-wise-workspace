// Package mp4 parses ISO BMFF box structure and extracts mfhd/tfdt/trun.
package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	bmp4 "github.com/abema/go-mp4"
)

// Parse errors. None of them are silently dropped: the caller records
// them into the report and continues with the remaining fragments.
var (
	ErrBoxOverflow = errors.New("mp4: box length exceeds available data")
	ErrBoxTooSmall = errors.New("mp4: box length smaller than header")
	ErrMissingMfhd = errors.New("mp4: moof without mfhd")
	ErrMissingTraf = errors.New("mp4: moof without traf")
	ErrDupMfhdBox  = errors.New("mp4: moof contains multiple mfhd boxes")
	ErrNoMoof      = errors.New("mp4: fragment contains no moof")
)

// Sample is one trun entry.
type Sample struct {
	Duration uint32
	Size     uint32
}

// Info is the parsed content of one CMAF/fMP4 fragment.
type Info struct {
	TrackID     uint32
	Seq         uint32 // mfhd sequence number
	TFDT        uint64
	TFDTVersion int
	Samples     []Sample
	Payload     []byte // mdat data
}

// Duration is the total sample duration in track timescale units.
func (i *Info) Duration() uint64 {
	var d uint64
	for _, s := range i.Samples {
		d += uint64(s.Duration)
	}
	return d
}

// BoxHeader is a top-level box header from the manual pre-walk.
type BoxHeader struct {
	Type   [4]byte
	Offset int
	Size   int // total box size including header
}

// walkBoxes validates top-level box headers against the buffer length.
// It is what turns "box length out of bounds" into a traceable error
// instead of a panic or a silently truncated read.
func walkBoxes(data []byte) ([]BoxHeader, error) {
	var out []BoxHeader
	off := 0
	for off < len(data) {
		if len(data)-off < 8 {
			return nil, fmt.Errorf("%w: truncated header at offset %d", ErrBoxOverflow, off)
		}
		size := int(binary.BigEndian.Uint32(data[off:]))
		var typ [4]byte
		copy(typ[:], data[off+4:off+8])
		hdr := 8
		if size == 1 {
			if len(data)-off < 16 {
				return nil, fmt.Errorf("%w: truncated largesize at offset %d", ErrBoxOverflow, off)
			}
			large := binary.BigEndian.Uint64(data[off+8:])
			if large > uint64(len(data)) {
				return nil, fmt.Errorf("%w: %s declares %d bytes at offset %d", ErrBoxOverflow, typ, large, off)
			}
			size = int(large)
			hdr = 16
		} else if size == 0 {
			size = len(data) - off // box extends to end of data
		}
		if size < hdr {
			return nil, fmt.Errorf("%w: %s size %d at offset %d", ErrBoxTooSmall, typ, size, off)
		}
		if off+size > len(data) {
			return nil, fmt.Errorf("%w: %s declares %d bytes at offset %d, only %d left",
				ErrBoxOverflow, typ, size, off, len(data)-off)
		}
		out = append(out, BoxHeader{Type: typ, Offset: off, Size: size})
		off += size
	}
	return out, nil
}

// Parse parses one fragment: styp?, moof(mfhd, traf(tfhd, tfdt, trun)), mdat.
func Parse(data []byte) (*Info, error) {
	if _, err := walkBoxes(data); err != nil {
		return nil, err
	}
	info := &Info{}
	var mdatPayload []byte
	mfhdCount := 0
	trafCount := 0

	_, err := bmp4.ReadBoxStructure(bytes.NewReader(data), func(h *bmp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case bmp4.BoxTypeMoof():
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
			return nil, nil
		case bmp4.BoxTypeMfhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			mfhdCount++
			info.Seq = box.(*bmp4.Mfhd).SequenceNumber
			return nil, nil
		case bmp4.BoxTypeTraf():
			trafCount++
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
			return nil, nil
		case bmp4.BoxTypeTfhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			info.TrackID = box.(*bmp4.Tfhd).TrackID
			return nil, nil
		case bmp4.BoxTypeTfdt():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tfdt := box.(*bmp4.Tfdt)
			info.TFDT = tfdt.GetBaseMediaDecodeTime()
			info.TFDTVersion = int(tfdt.GetVersion())
			return nil, nil
		case bmp4.BoxTypeTrun():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			trun := box.(*bmp4.Trun)
			for _, e := range trun.Entries {
				info.Samples = append(info.Samples, Sample{Duration: e.SampleDuration, Size: e.SampleSize})
			}
			return nil, nil
		case bmp4.BoxTypeMdat():
			var buf bytes.Buffer
			if _, err := h.ReadData(&buf); err != nil {
				return nil, err
			}
			mdatPayload = append(mdatPayload, buf.Bytes()...)
			return nil, nil
		}
		return nil, nil
	})
	if err != nil {
		return nil, fmt.Errorf("mp4: structure read: %w", err)
	}
	if mfhdCount == 0 {
		return nil, ErrMissingMfhd
	}
	if mfhdCount > 1 {
		return nil, ErrDupMfhdBox
	}
	if trafCount == 0 {
		return nil, ErrMissingTraf
	}
	info.Payload = mdatPayload
	return info, nil
}
