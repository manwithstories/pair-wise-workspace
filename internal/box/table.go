package box

import (
	"encoding/binary"
	"errors"
	"io"
)

// maxPayload caps a single box payload read into memory (moov is small).
const maxPayload = 64 << 20

// ErrNoMoov reports a file without a moov box.
var ErrNoMoov = errors.New("moov_not_found")

// SttsEntry maps sample count to decoding delta.
type SttsEntry struct{ Count, Delta uint32 }

// CttsEntry maps sample count to composition offset (signed, version 1).
type CttsEntry struct {
	Count  uint32
	Offset int64
}

// StscEntry maps chunks to samples-per-chunk and sample description.
type StscEntry struct{ FirstChunk, SamplesPerChunk, DescIndex uint32 }

// SampleTable holds the parsed stbl tables needed to index samples.
type SampleTable struct {
	Stts   []SttsEntry
	Ctts   []CttsEntry
	Stss   []uint32
	Sizes  []uint32
	Stsc   []StscEntry
	Chunks []uint64
}

// Trak is one parsed track.
type Trak struct {
	Handler       string
	Timescale     uint32
	MediaDuration uint64
	Elst          []ElstEntry
	Table         SampleTable
}

// Moov is the parsed movie box.
type Moov struct {
	Offset    uint64
	Timescale uint32
	Duration  uint64 // mvhd duration in movie timescale
	Traks     []*Trak
	BoxCount  int // total boxes in the whole file
}

// Payload reads a box payload into memory with a size cap.
func Payload(r io.ReaderAt, h Header) ([]byte, error) {
	if h.DataSize() > maxPayload {
		return nil, ErrBadSize
	}
	buf := make([]byte, h.DataSize())
	if _, err := r.ReadAt(buf, int64(h.DataOffset())); err != nil {
		return nil, err
	}
	return buf, nil
}

// FindMoov scans top-level boxes (moov may lead or trail), returning the
// moov header and the total box count of the file.
func FindMoov(r io.ReaderAt, fileSize uint64) (Header, int, error) {
	var moov Header
	total := 0
	err := Walk(r, 0, fileSize, func(h Header) error {
		n, err := CountBoxes(r, h.Offset, h.End(), 0)
		if err != nil {
			return err
		}
		total += n
		if h.Type == "moov" && moov.Size == 0 {
			moov = h
		}
		return nil
	})
	if err != nil {
		return Header{}, 0, err
	}
	if moov.Size == 0 {
		return Header{}, 0, ErrNoMoov
	}
	return moov, total, nil
}

// ParseMoov parses mvhd and every trak's mdia tables.
func ParseMoov(r io.ReaderAt, moov Header) (*Moov, error) {
	m := &Moov{Offset: moov.Offset}
	err := Walk(r, moov.DataOffset(), moov.End(), func(h Header) error {
		switch h.Type {
		case "mvhd":
			p, err := Payload(r, h)
			if err != nil {
				return err
			}
			m.Timescale, m.Duration, err = parseMvhd(p)
			return err
		case "trak":
			t, err := parseTrak(r, h)
			if err != nil {
				return err
			}
			m.Traks = append(m.Traks, t)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

func parseMvhd(p []byte) (uint32, uint64, error) {
	if len(p) < 4 {
		return 0, 0, ErrTableRange
	}
	if p[0] == 1 {
		if len(p) < 32 {
			return 0, 0, ErrTableRange
		}
		return binary.BigEndian.Uint32(p[20:24]), binary.BigEndian.Uint64(p[24:32]), nil
	}
	if len(p) < 20 {
		return 0, 0, ErrTableRange
	}
	return binary.BigEndian.Uint32(p[12:16]), uint64(binary.BigEndian.Uint32(p[16:20])), nil
}

func parseTrak(r io.ReaderAt, trak Header) (*Trak, error) {
	t := &Trak{}
	err := Walk(r, trak.DataOffset(), trak.End(), func(h Header) error {
		switch h.Type {
		case "edts":
			return Walk(r, ChildStart(h), h.End(), func(c Header) error {
				if c.Type != "elst" {
					return nil
				}
				p, err := Payload(r, c)
				if err != nil {
					return err
				}
				t.Elst, err = ParseElst(p)
				return err
			})
		case "mdia":
			return Walk(r, ChildStart(h), h.End(), func(c Header) error {
				switch c.Type {
				case "mdhd":
					p, err := Payload(r, c)
					if err != nil {
						return err
					}
					t.Timescale, t.MediaDuration, err = parseMvhd(p) // same layout
					return err
				case "hdlr":
					p, err := Payload(r, c)
					if err != nil {
						return err
					}
					if len(p) >= 12 {
						t.Handler = string(p[8:12])
					}
				case "minf":
					return parseMinf(r, c, t)
				}
				return nil
			})
		}
		return nil
	})
	return t, err
}

func parseMinf(r io.ReaderAt, minf Header, t *Trak) error {
	return Walk(r, ChildStart(minf), minf.End(), func(h Header) error {
		if h.Type != "stbl" {
			return nil
		}
		return Walk(r, ChildStart(h), h.End(), func(c Header) error {
			p, err := Payload(r, c)
			if err != nil {
				return err
			}
			return parseTable(c.Type, p, &t.Table)
		})
	})
}

func parseTable(typ string, p []byte, st *SampleTable) error {
	u32 := func(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
	count := func() (uint32, error) {
		if len(p) < 8 {
			return 0, ErrTableRange
		}
		return u32(p[4:8]), nil
	}
	check := func(n uint32, size int) error {
		if uint64(n)*uint64(size) > uint64(len(p)-8) {
			return ErrTableRange
		}
		return nil
	}
	switch typ {
	case "stts":
		n, err := count()
		if err != nil || check(n, 8) != nil {
			return ErrTableRange
		}
		for i, off := uint32(0), 8; i < n; i, off = i+1, off+8 {
			st.Stts = append(st.Stts, SttsEntry{u32(p[off : off+4]), u32(p[off+4 : off+8])})
		}
	case "ctts":
		n, err := count()
		if err != nil || check(n, 8) != nil {
			return ErrTableRange
		}
		v1 := p[0] == 1
		for i, off := uint32(0), 8; i < n; i, off = i+1, off+8 {
			raw := u32(p[off+4 : off+8])
			e := CttsEntry{Count: u32(p[off : off+4])}
			if v1 {
				e.Offset = int64(int32(raw))
			} else {
				e.Offset = int64(raw)
			}
			st.Ctts = append(st.Ctts, e)
		}
	case "stss":
		n, err := count()
		if err != nil || check(n, 4) != nil {
			return ErrTableRange
		}
		for i, off := uint32(0), 8; i < n; i, off = i+1, off+4 {
			st.Stss = append(st.Stss, u32(p[off:off+4]))
		}
	case "stsz":
		if len(p) < 12 {
			return ErrTableRange
		}
		size, n := u32(p[4:8]), u32(p[8:12])
		if size != 0 {
			st.Sizes = make([]uint32, n)
			for i := range st.Sizes {
				st.Sizes[i] = size
			}
			return nil
		}
		if uint64(n)*4 > uint64(len(p)-12) {
			return ErrTableRange
		}
		for i, off := uint32(0), 12; i < n; i, off = i+1, off+4 {
			st.Sizes = append(st.Sizes, u32(p[off:off+4]))
		}
	case "stsc":
		n, err := count()
		if err != nil || check(n, 12) != nil {
			return ErrTableRange
		}
		for i, off := uint32(0), 8; i < n; i, off = i+1, off+12 {
			st.Stsc = append(st.Stsc, StscEntry{u32(p[off : off+4]), u32(p[off+4 : off+8]), u32(p[off+8 : off+12])})
		}
	case "stco":
		n, err := count()
		if err != nil || check(n, 4) != nil {
			return ErrTableRange
		}
		for i, off := uint32(0), 8; i < n; i, off = i+1, off+4 {
			st.Chunks = append(st.Chunks, uint64(u32(p[off:off+4])))
		}
	case "co64":
		n, err := count()
		if err != nil || check(n, 8) != nil {
			return ErrTableRange
		}
		for i, off := uint32(0), 8; i < n; i, off = i+1, off+8 {
			st.Chunks = append(st.Chunks, binary.BigEndian.Uint64(p[off:off+8]))
		}
	}
	return nil
}
