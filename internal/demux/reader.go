// Package demux reads MP4/MOV boxes and sample tables with byte-precise
// validation. The reader never buffers media payloads; it builds a sample
// index and later streams sample bytes on demand.
package demux

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"mediamerge/internal/checksum"
	"mediamerge/internal/model"
)

// Box is a located box header.
type Box struct {
	Type       string
	Offset     int64
	HeaderSize int64
	Size       int64
}

// End returns the first byte after the box.
func (b Box) End() int64 { return b.Offset + b.Size }

// PayloadOffset returns the first payload byte.
func (b Box) PayloadOffset() int64 { return b.Offset + b.HeaderSize }

// Table is a raw sample table box retained for value-level validation.
type Table struct {
	Box
	Payload []byte
}

// Mdat locates the media data payload.
type Mdat struct {
	Offset       int64
	PayloadStart int64
	PayloadEnd   int64
}

// Track is one input track with its validated sample index.
type Track struct {
	model.Track
	Samples []model.SampleRef
}

// File is the complete index of an input file.
type File struct {
	Path           string
	Size           int64
	InputSHA       string
	MovieTimescale uint32
	Tracks         []*Track
	Mdat           Mdat
}

// Read parses path, validates structure and sample tables, and returns an
// index. It is safe for concurrent use by callers reading disjoint samples.
func Read(ctx context.Context, path string) (*File, error) {
	sha, size, err := checksum.File(path)
	if err != nil {
		return nil, derr(model.StageDemux, model.CodeTruncated, 0, 0, false, "open input: %v", err)
	}
	f := &File{Path: path, Size: size, InputSHA: sha}
	file, err := os.Open(path)
	if err != nil {
		return nil, derr(model.StageDemux, model.CodeTruncated, 0, 0, false, "open input: %v", err)
	}
	defer file.Close()

	var moovData []byte
	var moovBase int64
	var mdat *Box
	off := int64(0)
	for off < size {
		if err := ctx.Err(); err != nil {
			return nil, derr(model.StageDemux, model.CodeCanceled, off, 0, true, "%v", err)
		}
		if size-off < 8 {
			return nil, derr(model.StageDemux, model.CodeTruncated, size, 0, false,
				"truncated box header: %d trailing bytes", size-off)
		}
		b, perr := readBoxHeader(file, off, size)
		if perr != nil {
			return nil, perr
		}
		if b.End() > size {
			return nil, derr(model.StageDemux, model.CodeTruncated, size, 0, false,
				"box %q at %d declares end %d beyond file size %d", b.Type, b.Offset, b.End(), size)
		}
		switch b.Type {
		case "moov":
			if moovData != nil {
				return nil, derr(model.StageDemux, model.CodeBoxOverrun, b.Offset, 0, false, "duplicate moov box")
			}
			moovData = make([]byte, b.Size-b.HeaderSize)
			if _, err := file.ReadAt(moovData, b.PayloadOffset()); err != nil {
				return nil, derr(model.StageDemux, model.CodeTruncated, b.Offset, 0, false, "read moov: %v", err)
			}
			moovBase = b.PayloadOffset()
		case "mdat":
			if mdat != nil {
				return nil, derr(model.StageDemux, model.CodeBoxOverrun, b.Offset, 0, false, "duplicate mdat box")
			}
			mb := b
			mdat = &mb
		}
		off = b.End()
	}
	if moovData == nil {
		return nil, derr(model.StageDemux, model.CodeMissingBox, 0, 0, false, "missing moov box")
	}
	if mdat == nil {
		return nil, derr(model.StageDemux, model.CodeMissingBox, 0, 0, false, "missing mdat box")
	}
	f.Mdat = Mdat{Offset: mdat.Offset, PayloadStart: mdat.PayloadOffset(), PayloadEnd: mdat.End()}
	if err := f.parseMoov(moovData, moovBase); err != nil {
		return nil, err
	}
	if err := f.buildSamples(); err != nil {
		return nil, err
	}
	return f, nil
}

func derr(stage model.Stage, code string, offset int64, track uint32, recoverable bool, format string, args ...interface{}) *model.Error {
	return model.NewError(stage, code, offset, track, recoverable, format, args...)
}

// readBoxHeader reads a 32/64-bit sized box header at off.
func readBoxHeader(r io.ReaderAt, off, fileSize int64) (Box, error) {
	var buf [8]byte
	if _, err := r.ReadAt(buf[:], off); err != nil {
		return Box{}, derr(model.StageDemux, model.CodeTruncated, fileSize, 0, false, "read box header at %d: %v", off, err)
	}
	size := int64(binary.BigEndian.Uint32(buf[0:4]))
	typ := string(buf[4:8])
	hdr := int64(8)
	if size == 1 {
		var ext [8]byte
		if _, err := r.ReadAt(ext[:], off+8); err != nil {
			return Box{}, derr(model.StageDemux, model.CodeTruncated, fileSize, 0, false,
				"read large box size for %q at %d: %v", typ, off, err)
		}
		size = int64(binary.BigEndian.Uint64(ext[:]))
		hdr = 16
	} else if size == 0 {
		size = fileSize - off
	}
	if size < hdr {
		return Box{}, derr(model.StageDemux, model.CodeBoxOverrun, off, 0, false,
			"box %q at %d has invalid size %d", typ, off, size)
	}
	return Box{Type: typ, Offset: off, HeaderSize: hdr, Size: size}, nil
}

// splitBoxes walks sibling boxes inside data. It reports the first byte that
// cannot be parsed when a box overruns its parent.
func splitBoxes(data []byte, base int64) ([]Table, error) {
	var out []Table
	off := int64(0)
	n := int64(len(data))
	for off < n {
		if n-off < 8 {
			return nil, derr(model.StageDemux, model.CodeBoxOverrun, base+off, 0, false,
				"truncated child box header at %d", base+off)
		}
		size := int64(binary.BigEndian.Uint32(data[off : off+4]))
		typ := string(data[off+4 : off+8])
		hdr := int64(8)
		if size == 1 {
			if n-off < 16 {
				return nil, derr(model.StageDemux, model.CodeBoxOverrun, base+off, 0, false,
					"truncated large child box header at %d", base+off)
			}
			size = int64(binary.BigEndian.Uint64(data[off+8 : off+16]))
			hdr = 16
		} else if size == 0 {
			size = n - off
		}
		if size < hdr {
			return nil, derr(model.StageDemux, model.CodeBoxOverrun, base+off, 0, false,
				"child box %q at %d has invalid size %d", typ, base+off, size)
		}
		if off+size > n {
			return nil, derr(model.StageDemux, model.CodeBoxOverrun, base+off, 0, false,
				"child box %q at %d declares end %d beyond parent end %d", typ, base+off, base+off+size, base+n)
		}
		out = append(out, Table{
			Box:     Box{Type: typ, Offset: base + off, HeaderSize: hdr, Size: size},
			Payload: data[off+hdr : off+size],
		})
		off += size
	}
	return out, nil
}

func (f *File) parseMoov(data []byte, base int64) error {
	children, err := splitBoxes(data, base)
	if err != nil {
		return err
	}
	seenMoov := false
	for _, c := range children {
		switch c.Type {
		case "mvhd":
			if ts, err := readTimescale(c.Payload, 12, 20); err == nil {
				f.MovieTimescale = ts
			}
		case "trak":
			seenMoov = true
			t, err := f.parseTrak(c)
			if err != nil {
				return err
			}
			if t != nil {
				f.Tracks = append(f.Tracks, t)
			}
		case "udta", "meta", "free", "skip", "mvex", "iods", "uuid":
			// tolerated non-essential top-level movie boxes
		default:
			// Unknown movie-level boxes are not required for remuxing.
		}
	}
	if !seenMoov {
		return derr(model.StageDemux, model.CodeMissingBox, base, 0, false, "moov contains no trak box")
	}
	return nil
}

func readTimescale(payload []byte, v0Off, v1Off int) (uint32, error) {
	if len(payload) < 4 {
		return 0, fmt.Errorf("short full box")
	}
	ver := payload[0]
	if ver == 1 {
		if len(payload) < v1Off+4 {
			return 0, fmt.Errorf("short v1 timescale")
		}
		return binary.BigEndian.Uint32(payload[v1Off : v1Off+4]), nil
	}
	if len(payload) < v0Off+4 {
		return 0, fmt.Errorf("short v0 timescale")
	}
	return binary.BigEndian.Uint32(payload[v0Off : v0Off+4]), nil
}
