// Package mp4 scans ISO BMFF / QuickTime containers, localizes the first
// anomalous byte of malformed boxes and extracts track metadata through
// go-mp4. The byte-level scanner is hand rolled so corruption is reported
// with a precise offset instead of a generic parser failure.
package mp4

import (
	"encoding/binary"
	"io"

	"medpipe/internal/clock"
)

// Limits rejects inputs that would break the bounded-memory contract.
type Limits struct {
	MaxFileBytes int64
	MaxBoxBytes  int64
	MaxDepth     int
	// MaxSampleBytes caps a single track sample so a pathological frame or
	// packet cannot blow the bounded-memory guarantee.
	MaxSampleBytes uint32
}

// DefaultLimits are conservative caps for mobile-recorded clips.
var DefaultLimits = Limits{
	MaxFileBytes:   8 << 30, // 8 GiB
	MaxBoxBytes:    4 << 30, // 4 GiB per box
	MaxDepth:       16,
	MaxSampleBytes: 64 << 20, // 64 MiB per sample
}

// BoxRecord is one parsed box header with its exact file position.
type BoxRecord struct {
	Type       string
	Offset     int64 // absolute byte offset of the size field
	HeaderSize int64 // 8 (32-bit size) or 16 (64-bit largesize)
	Size       int64 // declared total size in bytes
	Children   []*BoxRecord
}

// PayloadOffset is the first byte after the box header.
func (b *BoxRecord) PayloadOffset() int64 { return b.Offset + b.HeaderSize }

// End returns the absolute offset one past the box's last byte.
func (b *BoxRecord) End() int64 { return b.Offset + b.Size }

// Result is the outcome of a successful structural scan.
type Result struct {
	FileSize int64
	Boxes    []*BoxRecord
	Ftyp     *BoxRecord
	Mdat     []*BoxRecord
}

var containers = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
	"edts": true, "dinf": true, "udta": true, "moof": true, "traf": true,
	"mvex": true, "iref": true, "iprp": true, "ipco": true, "sinf": true,
	"schi": true, "ilst": true, "meta": true,
}

// fullBoxes carry a 4-byte version/flags prefix before child boxes.
var fullBoxes = map[string]bool{"meta": true}

// Scan walks every box in r and localizes corruption. On failure it
// returns a *clock.Error (KindCorrupt) whose Offset is the first
// anomalous byte, exact to one byte.
func Scan(r io.ReadSeeker, lim Limits) (*Result, error) {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, clock.E(clock.KindOther, "mp4.scan", clock.WithWrap(err))
	}
	if size > lim.MaxFileBytes {
		return nil, clock.E(clock.KindArgument, "mp4.scan",
			clock.WithDetail("file %d bytes exceeds limit %d", size, lim.MaxFileBytes))
	}
	boxes, err := scanChildren(r, 0, size, 0, lim)
	if err != nil {
		return nil, err
	}
	res := &Result{FileSize: size, Boxes: boxes}
	walk(boxes, func(b *BoxRecord) {
		switch b.Type {
		case "ftyp":
			if res.Ftyp == nil {
				res.Ftyp = b
			}
		case "mdat":
			res.Mdat = append(res.Mdat, b)
		}
	})
	return res, nil
}

func scanChildren(r io.ReadSeeker, start, end, depth int64, lim Limits) ([]*BoxRecord, error) {
	if int(depth) > lim.MaxDepth {
		return nil, clock.E(clock.KindCorrupt, "mp4.scan", clock.WithOffset(start),
			clock.WithDetail("box nesting exceeds depth %d", lim.MaxDepth))
	}
	var out []*BoxRecord
	pos := start
	for pos < end {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return nil, clock.E(clock.KindOther, "mp4.scan", clock.WithWrap(err))
		}
		hdr := make([]byte, 16)
		n, err := io.ReadFull(r, hdr[:8])
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			// Header begins here but the file ends inside it.
			return nil, clock.E(clock.KindCorrupt, "mp4.scan", clock.WithOffset(pos+int64(n)),
				clock.WithDetail("truncated box header: %d of 8 header bytes", n))
		}
		if err != nil {
			return nil, clock.E(clock.KindOther, "mp4.scan", clock.WithWrap(err))
		}
		size32 := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		rec := &BoxRecord{Type: typ, Offset: pos, HeaderSize: 8}

		switch size32 {
		case 1: // 64-bit largesize
			n2, err := io.ReadFull(r, hdr[8:16])
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				return nil, clock.E(clock.KindCorrupt, "mp4.scan",
					clock.WithOffset(pos+8+int64(n2)),
					clock.WithDetail("truncated 64-bit box header"))
			}
			if err != nil {
				return nil, clock.E(clock.KindOther, "mp4.scan", clock.WithWrap(err))
			}
			large := int64(binary.BigEndian.Uint64(hdr[8:16]))
			if large < 16 {
				return nil, clock.E(clock.KindCorrupt, "mp4.scan", clock.WithOffset(pos),
					clock.WithDetail("illegal largesize %d (<16)", large))
			}
			rec.HeaderSize, rec.Size = 16, large
		case 0: // box extends to end of file / parent
			rec.Size = end - pos
		default:
			if size32 < 8 {
				return nil, clock.E(clock.KindCorrupt, "mp4.scan", clock.WithOffset(pos),
					clock.WithDetail("illegal box length %d (<8)", size32))
			}
			rec.Size = size32
		}
		if rec.Size > lim.MaxBoxBytes {
			return nil, clock.E(clock.KindArgument, "mp4.scan", clock.WithOffset(pos),
				clock.WithDetail("box %s size %d exceeds limit %d", typ, rec.Size, lim.MaxBoxBytes))
		}
		boxEnd := pos + rec.Size
		if boxEnd > end {
			// First byte the box claims but the file does not contain.
			return nil, clock.E(clock.KindCorrupt, "mp4.scan", clock.WithOffset(end),
				clock.WithDetail("box %s at %d declares end %d past boundary %d",
					typ, pos, boxEnd, end))
		}
		if containers[typ] {
			payloadStart := rec.PayloadOffset()
			if fullBoxes[typ] {
				payloadStart += 4
			}
			kids, err := scanChildren(r, payloadStart, boxEnd, depth+1, lim)
			if err != nil {
				return nil, err
			}
			rec.Children = kids
		}
		out = append(out, rec)
		pos = boxEnd
	}
	return out, nil
}

func walk(boxes []*BoxRecord, fn func(*BoxRecord)) {
	for _, b := range boxes {
		fn(b)
		walk(b.Children, fn)
	}
}

// Find returns every box matching the given type at any depth.
func (r *Result) Find(typ string) []*BoxRecord {
	var hits []*BoxRecord
	walk(r.Boxes, func(b *BoxRecord) {
		if b.Type == typ {
			hits = append(hits, b)
		}
	})
	return hits
}
