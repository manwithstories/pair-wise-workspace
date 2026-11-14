// Package mp4atom parses the leaf tables inside a scanned moov box and
// expands them into flat per-sample tables. All parsing is self-contained:
// the go-mp4 dependency is used only on the fixture-generation side.
package mp4atom

import (
	"encoding/binary"
	"io"

	"mp4replay/internal/boxscan"
	"mp4replay/internal/report"
)

// Raw is a big-endian reader over an in-memory leaf box payload.
type Raw struct {
	buf []byte
	pos int
}

func newRaw(buf []byte) *Raw { return &Raw{buf: buf} }

func (r *Raw) left() int { return len(r.buf) - r.pos }

func (r *Raw) u8() (uint8, error) {
	if r.left() < 1 {
		return 0, short("u8")
	}
	v := r.buf[r.pos]
	r.pos++
	return v, nil
}

func (r *Raw) u16() (uint16, error) {
	if r.left() < 2 {
		return 0, short("u16")
	}
	v := binary.BigEndian.Uint16(r.buf[r.pos:])
	r.pos += 2
	return v, nil
}

func (r *Raw) i16() (int16, error) {
	v, err := r.u16()
	return int16(v), err
}

func (r *Raw) u24() (uint32, error) {
	if r.left() < 3 {
		return 0, short("u24")
	}
	v := uint32(r.buf[r.pos])<<16 | uint32(r.buf[r.pos+1])<<8 |
		uint32(r.buf[r.pos+2])
	r.pos += 3
	return v, nil
}

func (r *Raw) u32() (uint32, error) {
	if r.left() < 4 {
		return 0, short("u32")
	}
	v := binary.BigEndian.Uint32(r.buf[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *Raw) u64() (uint64, error) {
	if r.left() < 8 {
		return 0, short("u64")
	}
	v := binary.BigEndian.Uint64(r.buf[r.pos:])
	r.pos += 8
	return v, nil
}

func (r *Raw) i32() (int32, error) {
	v, err := r.u32()
	return int32(v), err
}

func (r *Raw) i64() (int64, error) {
	v, err := r.u64()
	return int64(v), err
}

// fullBox reads and validates the version/flags FullBox header.
func (r *Raw) fullBox() (uint8, uint32, error) {
	ver, err := r.u8()
	if err != nil {
		return 0, 0, err
	}
	flags, err := r.u24()
	return ver, flags, err
}

func short(field string) error {
	return report.NewError(report.ExitBoxInvalid, "parse",
		"truncated box while reading "+field)
}

// Edit is one elst entry. MediaTime is -1 for an empty edit.
type Edit struct {
	SegmentDuration uint64
	MediaTime       int64
	Rate            float64
}

// Table holds every parsed leaf box for one trak.
type Table struct {
	TrackID   uint32
	Timescale uint32
	// MovieTS is the mvhd timescale, needed for elst segment durations.
	MovieTS   uint32
	MediaDur  uint64
	Handler   string
	Language  uint16
	Stts      []SttsEntry
	Ctts      []CttsEntry
	CttsVer   uint8
	Stss      []uint32 // 1-based sample numbers; empty means all sync
	Stsc      []StscEntry
	StszSizes []uint32
	StszFixed uint32
	StszCount uint32
	Stco      []int64
	Elst      []Edit
	HasCtts   bool
	HasStss   bool
}

// SttsEntry mirrors an stts run.
type SttsEntry struct {
	Count uint32
	Delta uint32
}

// CttsEntry mirrors a ctts run; Offset is signed regardless of version.
type CttsEntry struct {
	Count  uint32
	Offset int64
}

// StscEntry mirrors an stsc run (1-based first chunk).
type StscEntry struct {
	FirstChunk      uint32
	SamplesPerChunk uint32
	DescIndex       uint32
}

// MovieInfo is the parsed mvhd.
type MovieInfo struct {
	Timescale uint32
	Duration  uint64
	Version   uint8
}

func loadBox(ra io.ReaderAt, b *boxscan.Box) (*Raw, error) {
	buf, err := boxscan.ReadPayload(ra, b)
	if err != nil {
		return nil, err
	}
	return newRaw(buf), nil
}

// ParseMovie reads mvhd from a scanned moov box.
func ParseMovie(ra io.ReaderAt, moov *boxscan.Box) (*MovieInfo, error) {
	mvhd := boxscan.Path(moov, "mvhd")
	if mvhd == nil {
		return nil, report.NewError(report.ExitBoxInvalid, "parse", "mvhd missing")
	}
	r, err := loadBox(ra, mvhd)
	if err != nil {
		return nil, err
	}
	ver, _, err := r.fullBox()
	if err != nil {
		return nil, err
	}
	mi := &MovieInfo{Version: ver}
	if ver == 1 {
		if _, err = r.u64(); err != nil {
			return nil, err
		}
		if _, err = r.u64(); err != nil {
			return nil, err
		}
	} else {
		if _, err = r.u32(); err != nil {
			return nil, err
		}
		if _, err = r.u32(); err != nil {
			return nil, err
		}
	}
	if mi.Timescale, err = r.u32(); err != nil {
		return nil, err
	}
	if ver == 1 {
		mi.Duration, err = r.u64()
	} else {
		var d uint32
		d, err = r.u32()
		mi.Duration = uint64(d)
	}
	return mi, err
}
