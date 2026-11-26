// Package segment parses fMP4 fragment structure and AVC/HEVC sample
// headers to locate keyframes. A truncated box or missing sample data is
// reported as a typed error so callers can mark only the affected range.
package segment

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// TruncatedError reports a box (or sample payload) whose declared size
// extends past the available data.
type TruncatedError struct {
	Offset   int64
	BoxType  string
	Declared int64
	Remain   int64
}

func (e *TruncatedError) Error() string {
	return fmt.Sprintf("truncated %s box at offset %d: declares %d bytes, %d remain",
		e.BoxType, e.Offset, e.Declared, e.Remain)
}

// IsTruncated reports whether err is (or wraps) a TruncatedError.
func IsTruncated(err error) bool {
	var te *TruncatedError
	return errors.As(err, &te)
}

// Sample describes one entry of a trun box after applying tfhd defaults.
type Sample struct {
	Duration uint32
	Size     uint32
	Flags    uint32
	CTO      int32
}

// IsSync reports the sample's sync bit (inverse of
// sample_is_non_sync_sample, flag bit 16).
func (s Sample) IsSync() bool { return s.Flags&0x10000 == 0 }

// Fragment is the parsed structure of one fMP4 media segment.
type Fragment struct {
	Boxes          []string // top-level box types, in order
	TrackID        uint32
	BaseDecodeTime uint64
	HasTfdt        bool
	Samples        []Sample
	MdatOffset     int64 // absolute offset of the mdat payload
	MdatSize       int64
}

type boxHeader struct {
	typ   string
	off   int64
	size  int64 // total size including header
	hsize int64
}

func readBox(data []byte, off int64) (boxHeader, error) {
	remain := int64(len(data)) - off
	if remain < 8 {
		return boxHeader{}, &TruncatedError{Offset: off, BoxType: "????", Declared: 8, Remain: remain}
	}
	size := int64(binary.BigEndian.Uint32(data[off:]))
	typ := string(data[off+4 : off+8])
	h := int64(8)
	switch size {
	case 1:
		if remain < 16 {
			return boxHeader{}, &TruncatedError{Offset: off, BoxType: typ, Declared: 16, Remain: remain}
		}
		size = int64(binary.BigEndian.Uint64(data[off+8:]))
		h = 16
	case 0:
		size = remain // box extends to end of data
	}
	if size < h {
		return boxHeader{}, fmt.Errorf("invalid %s box at offset %d: size %d < header %d", typ, off, size, h)
	}
	if size > remain {
		return boxHeader{}, &TruncatedError{Offset: off, BoxType: typ, Declared: size, Remain: remain}
	}
	return boxHeader{typ: typ, off: off, size: size, hsize: h}, nil
}

// need verifies that n bytes are available inside a box payload.
func need(b boxHeader, have, n int64, what string) error {
	if have < n {
		return &TruncatedError{Offset: b.off, BoxType: b.typ + "/" + what, Declared: n, Remain: have}
	}
	return nil
}

// ParseFragment walks the top-level boxes of an fMP4 segment and extracts
// the moof/traf metadata and mdat location. It fails on the first
// structural error; callers decide how to attribute the damage.
func ParseFragment(data []byte) (*Fragment, error) {
	f := &Fragment{}
	for off := int64(0); off < int64(len(data)); {
		b, err := readBox(data, off)
		if err != nil {
			return f, err
		}
		f.Boxes = append(f.Boxes, b.typ)
		payload := data[b.off+b.hsize : b.off+b.size]
		switch b.typ {
		case "moof":
			if err := parseMoof(payload, b.off+b.hsize, f); err != nil {
				return f, err
			}
		case "mdat":
			f.MdatOffset = b.off + b.hsize
			f.MdatSize = b.size - b.hsize
		}
		off += b.size
	}
	return f, nil
}

func parseMoof(data []byte, base int64, f *Fragment) error {
	for off := int64(0); off < int64(len(data)); {
		b, err := readBox(data, off)
		if err != nil {
			return err
		}
		if b.typ == "traf" {
			if err := parseTraf(data[b.off+b.hsize:b.off+b.size], f); err != nil {
				return err
			}
		}
		off += b.size
	}
	return nil
}

func parseTraf(data []byte, f *Fragment) error {
	var defDur, defSize, defFlags uint32
	for off := int64(0); off < int64(len(data)); {
		b, err := readBox(data, off)
		if err != nil {
			return err
		}
		p := data[b.off+b.hsize : b.off+b.size]
		switch b.typ {
		case "tfhd":
			if err := parseTfhd(b, p, f, &defDur, &defSize, &defFlags); err != nil {
				return err
			}
		case "tfdt":
			if err := parseTfdt(b, p, f); err != nil {
				return err
			}
		case "trun":
			if err := parseTrun(b, p, f, defDur, defSize, defFlags); err != nil {
				return err
			}
		}
		off += b.size
	}
	return nil
}

func parseTfhd(b boxHeader, p []byte, f *Fragment, defDur, defSize, defFlags *uint32) error {
	if err := need(b, int64(len(p)), 8, "tfhd"); err != nil {
		return err
	}
	flags := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
	f.TrackID = binary.BigEndian.Uint32(p[4:])
	q := p[8:]
	if flags&0x000001 != 0 { // base-data-offset-present
		if err := need(b, int64(len(q)), 8, "tfhd"); err != nil {
			return err
		}
		q = q[8:]
	}
	if flags&0x000002 != 0 { // sample-description-index
		q = q[4:]
	}
	read := func(dst *uint32) error {
		if err := need(b, int64(len(q)), 4, "tfhd"); err != nil {
			return err
		}
		*dst = binary.BigEndian.Uint32(q)
		q = q[4:]
		return nil
	}
	if flags&0x000008 != 0 {
		if err := read(defDur); err != nil {
			return err
		}
	}
	if flags&0x000010 != 0 {
		if err := read(defSize); err != nil {
			return err
		}
	}
	if flags&0x000020 != 0 {
		if err := read(defFlags); err != nil {
			return err
		}
	}
	return nil
}

func parseTfdt(b boxHeader, p []byte, f *Fragment) error {
	if err := need(b, int64(len(p)), 4, "tfdt"); err != nil {
		return err
	}
	version := p[0]
	f.HasTfdt = true
	if version == 1 {
		if err := need(b, int64(len(p)), 12, "tfdt"); err != nil {
			return err
		}
		f.BaseDecodeTime = binary.BigEndian.Uint64(p[4:])
	} else {
		if err := need(b, int64(len(p)), 8, "tfdt"); err != nil {
			return err
		}
		f.BaseDecodeTime = uint64(binary.BigEndian.Uint32(p[4:]))
	}
	return nil
}

func parseTrun(b boxHeader, p []byte, f *Fragment, defDur, defSize, defFlags uint32) error {
	if err := need(b, int64(len(p)), 8, "trun"); err != nil {
		return err
	}
	version := p[0]
	flags := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
	count := binary.BigEndian.Uint32(p[4:])
	q := p[8:]
	if flags&0x000001 != 0 { // data-offset-present
		if err := need(b, int64(len(q)), 4, "trun"); err != nil {
			return err
		}
		q = q[4:]
	}
	if flags&0x000004 != 0 { // first-sample-flags-present
		if err := need(b, int64(len(q)), 4, "trun"); err != nil {
			return err
		}
		q = q[4:]
	}
	durOK := flags&0x000100 != 0
	sizeOK := flags&0x000200 != 0
	flagsOK := flags&0x000400 != 0
	ctoOK := flags&0x000800 != 0
	for i := uint32(0); i < count; i++ {
		s := Sample{Duration: defDur, Size: defSize, Flags: defFlags}
		take := func(dst *uint32) error {
			if err := need(b, int64(len(q)), 4, "trun"); err != nil {
				return err
			}
			*dst = binary.BigEndian.Uint32(q)
			q = q[4:]
			return nil
		}
		if durOK {
			if err := take(&s.Duration); err != nil {
				return err
			}
		}
		if sizeOK {
			if err := take(&s.Size); err != nil {
				return err
			}
		}
		if flagsOK {
			if err := take(&s.Flags); err != nil {
				return err
			}
		}
		if ctoOK {
			var v uint32
			if err := take(&v); err != nil {
				return err
			}
			s.CTO = int32(v)
			if version == 0 {
				s.CTO = int32(v) // unsigned composition offset, kept as-is
			}
		}
		f.Samples = append(f.Samples, s)
	}
	return nil
}
