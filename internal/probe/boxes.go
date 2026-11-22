package probe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"mediacheck/internal/fault"
)

const (
	defaultMaxMoovBytes = 64 << 20
	defaultReadBuffer   = 1 << 20
	maxBoxCount         = 200000
)

// Box is one container box with absolute file coordinates.
type Box struct {
	Type   string `json:"type"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	Header int64  `json:"header"`
}

// Bounds is a half-open byte range inside the file.
type Bounds struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// container is the bounded sequential view of a media file.
type container struct {
	file        *os.File
	size        int64
	boxes       []Box
	mdats       []Bounds
	moofCount   int
	moovBox     *Box
	moovPayload []byte
	moovStart   int64
	truncated   bool
	firstUnread int64
	discarded   int64
	faults      []*fault.Fault
	maxDecode   int64
}

func openContainer(path string, cfg Config) (*container, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fault.Wrap(fault.CodeIO, fault.SeverityFatal, "open", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fault.Wrap(fault.CodeIO, fault.SeverityFatal, "stat", err)
	}
	if st.IsDir() {
		f.Close()
		return nil, fault.New(fault.CodeInvalidArgument, fault.SeverityFatal, "open", "input is a directory")
	}
	c := &container{file: f, size: st.Size(), firstUnread: -1, maxDecode: cfg.MaxDecodeBytes}
	if err := c.scan(cfg); err != nil {
		f.Close()
		return nil, err
	}
	return c, nil
}

func (c *container) Close() error {
	if c.file == nil {
		return nil
	}
	err := c.file.Close()
	c.file = nil
	return err
}

// scan walks top-level boxes without reading mdat payloads. A file of any
// size traverses with O(1) memory apart from the capped moov payload.
func (c *container) scan(cfg Config) error {
	var pos int64
	for pos < c.size {
		if len(c.boxes) >= maxBoxCount {
			c.addFault(fault.At(fault.CodeStructure, fault.SeverityPartial, "scan", pos, "too many top-level boxes"))
			break
		}
		if c.size-pos < 8 {
			c.markTruncated(pos, c.size-pos, "trailing bytes do not hold a box header")
			break
		}
		var hdr [16]byte
		if err := readFullAt(c.file, hdr[:8], pos); err != nil {
			return fault.At(fault.CodeIO, fault.SeverityFatal, "scan", pos, err.Error())
		}
		size32 := binary.BigEndian.Uint32(hdr[:4])
		typ := string(hdr[4:8])
		header := int64(8)
		boxSize := int64(size32)
		switch size32 {
		case 0:
			boxSize = c.size - pos
		case 1:
			if c.size-pos < 16 {
				c.markTruncated(pos, 16-(c.size-pos), "extended box header cut off")
				pos = c.size
				continue
			}
			if err := readFullAt(c.file, hdr[8:16], pos+8); err != nil {
				return fault.At(fault.CodeIO, fault.SeverityFatal, "scan", pos+8, err.Error())
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			header = 16
			if boxSize < 16 {
				c.addFault(fault.At(fault.CodeStructure, fault.SeverityFatal, "scan", pos,
					fmt.Sprintf("extended box %q declares size %d", typ, boxSize)))
				return fault.At(fault.CodeStructure, fault.SeverityFatal, "scan", pos, "invalid extended box size")
			}
		default:
			if size32 < 8 {
				c.addFault(fault.At(fault.CodeStructure, fault.SeverityFatal, "scan", pos,
					fmt.Sprintf("box %q declares size %d", typ, size32)))
				return fault.At(fault.CodeStructure, fault.SeverityFatal, "scan", pos, "invalid box size")
			}
		}
		b := Box{Type: typ, Offset: pos, Size: boxSize, Header: header}
		if boxSize > c.size-pos {
			if typ == "moov" {
				b.Size = c.size - pos
				c.boxes = append(c.boxes, b)
				c.loadMoov(b, cfg)
			} else if typ == "mdat" {
				c.mdats = append(c.mdats, Bounds{Start: pos + header, End: c.size})
			}
			c.addFault(fault.At(fault.CodeTruncated, fault.SeverityPartial, "scan", c.size,
				fmt.Sprintf("box %q declares %d bytes, only %d readable", typ, boxSize, c.size-pos)))
			c.truncated = true
			c.firstUnread = c.size
			c.discarded = pos + boxSize - c.size
			break
		}
		c.boxes = append(c.boxes, b)
		switch typ {
		case "moov":
			c.loadMoov(b, cfg)
		case "mdat":
			c.mdats = append(c.mdats, Bounds{Start: pos + header, End: pos + boxSize})
		case "moof":
			c.moofCount++
		}
		pos += boxSize
	}
	return nil
}

func (c *container) markTruncated(at, discarded int64, detail string) {
	c.truncated = true
	c.firstUnread = at
	c.discarded = discarded
	c.addFault(fault.At(fault.CodeTruncated, fault.SeverityPartial, "scan", at, detail))
}

// loadMoov reads at most MaxMoovBytes of the moov payload. A cut-off moov
// still yields whatever complete child boxes are present.
func (c *container) loadMoov(b Box, cfg Config) {
	start := b.Offset + b.Header
	avail := b.Size - b.Header
	if avail < 0 {
		avail = 0
	}
	want := avail
	if want > cfg.MaxMoovBytes {
		want = cfg.MaxMoovBytes
		c.addFault(fault.At(fault.CodeStructure, fault.SeverityPartial, "moov", start,
			fmt.Sprintf("moov payload %d exceeds cap %d; index may be incomplete", avail, cfg.MaxMoovBytes)))
	}
	buf := make([]byte, want)
	if want > 0 {
		if err := readFullAt(c.file, buf, start); err != nil {
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				c.addFault(fault.At(fault.CodeIO, fault.SeverityPartial, "moov", start, err.Error()))
			}
			readable := c.size - start
			if readable < 0 {
				readable = 0
			}
			if readable < want {
				buf = buf[:int(readable)]
			}
		}
	}
	c.moovBox = &b
	c.moovPayload = buf
	c.moovStart = start
}

func (c *container) addFault(f *fault.Fault) {
	c.faults = append(c.faults, f)
}

// childBox is a parsed child box whose payload aliases the parent buffer.
type childBox struct {
	Type    string
	Offset  int64 // absolute file offset of the box header
	Header  int64
	Size    int64
	Payload []byte
}

func (b childBox) payloadStart() int64 { return b.Offset + b.Header }

// children parses the immediate children of buf. Malformed tails produce a
// structure fault and stop the walk instead of reading out of bounds.
func (c *container) children(buf []byte, absBase int64, op string) []childBox {
	var out []childBox
	var pos int64
	for pos < int64(len(buf)) {
		if int64(len(buf))-pos < 8 {
			c.addFault(fault.At(fault.CodeStructure, fault.SeverityPartial, op, absBase+pos,
				fmt.Sprintf("trailing %d bytes inside box", int64(len(buf))-pos)))
			break
		}
		off := int(pos)
		size32 := binary.BigEndian.Uint32(buf[off : off+4])
		typ := string(buf[off+4 : off+8])
		header := int64(8)
		size := int64(size32)
		switch size32 {
		case 0:
			size = int64(len(buf)) - pos
		case 1:
			if int64(len(buf))-pos < 16 {
				c.addFault(fault.At(fault.CodeStructure, fault.SeverityPartial, op, absBase+pos, "extended header cut off"))
				return out
			}
			size = int64(binary.BigEndian.Uint64(buf[off+8 : off+16]))
			header = 16
			if size < 16 {
				c.addFault(fault.At(fault.CodeStructure, fault.SeverityPartial, op, absBase+pos, "invalid extended size"))
				return out
			}
		default:
			if size32 < 8 {
				c.addFault(fault.At(fault.CodeStructure, fault.SeverityPartial, op, absBase+pos, "invalid box size"))
				return out
			}
		}
		if size > int64(len(buf))-pos {
			c.addFault(fault.At(fault.CodeStructure, fault.SeverityPartial, op, absBase+pos,
				fmt.Sprintf("child box %q overruns parent", typ)))
			return out
		}
		out = append(out, childBox{
			Type:    typ,
			Offset:  absBase + pos,
			Header:  header,
			Size:    size,
			Payload: buf[pos+header : pos+size],
		})
		pos += size
	}
	return out
}

func (c *container) findChild(list []childBox, typ string) (childBox, bool) {
	for _, b := range list {
		if b.Type == typ {
			return b, true
		}
	}
	return childBox{}, false
}

func readFullAt(f *os.File, p []byte, off int64) error {
	if off < 0 {
		return fmt.Errorf("negative offset %d", off)
	}
	n, err := f.ReadAt(p, off)
	if err != nil && n < len(p) {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	if n < len(p) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// sampleReader performs bounded reads with one reusable buffer. Offsets and
// lengths are validated against the file size before any read is issued.
type sampleReader struct {
	file *os.File
	size int64
	buf  []byte
	max  int
}

func newSampleReader(file *os.File, size int64, max int) *sampleReader {
	if max <= 0 {
		max = defaultReadBuffer
	}
	return &sampleReader{file: file, size: size, max: max}
}

func (r *sampleReader) read(off int64, n int) ([]byte, error) {
	if off < 0 || n < 0 || off > r.size || int64(n) > r.size-off {
		return nil, fmt.Errorf("range [%d,%d) outside file of %d bytes", off, off+int64(n), r.size)
	}
	if n > r.max {
		return nil, fmt.Errorf("sample size %d exceeds buffer cap %d", n, r.max)
	}
	if cap(r.buf) < n {
		r.buf = make([]byte, n)
	}
	buf := r.buf[:n]
	if err := readFullAt(r.file, buf, off); err != nil {
		return nil, err
	}
	return buf, nil
}
