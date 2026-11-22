// Package stream analyses the MP4 container structure and keyframe (GOP)
// statistics. Container defects are located by byte offset and mapped onto
// the shared media time base.
package stream

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"mediascan/internal/fault"
)

// Box is one validated top-level (or moov child) container box.
type Box struct {
	Type   string
	Offset int64
	Size   int64
	Depth  int
}

// Analysis is the structural scan result.
type Analysis struct {
	Boxes       []Box
	MdatOffsets []int64 // mdat payload start offsets in file order
	Faults      []fault.Fault
	FileSize    int64
}

// requiredTopLevel boxes must each appear exactly once at depth 0.
var requiredTopLevel = map[string]bool{"ftyp": true, "moov": true, "mdat": true}

// Scan reads every MP4 box header with go-mp4 and validates boundaries
// against the actual file size. Truncated boxes and illegal sizes are
// located to their byte offset instead of being silently accepted.
func Scan(r io.ReadSeeker) (*Analysis, error) {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if size <= 0 {
		return nil, errors.New("mp4: empty file")
	}
	a := &Analysis{FileSize: size}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	seen := map[string]int{}
	var pos int64
	for pos < size {
		bi, err := mp4.ReadBoxInfo(r)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				a.Faults = append(a.Faults, fault.Fault{
					Cat: fault.CatTruncatedBox, Byte: pos,
					Ref: "?", Msg: "box header truncated by EOF",
				})
				return a, nil
			}
			return nil, err
		}
		boxEnd := int64(bi.Offset + bi.Size)
		ref := bi.Type.String()
		if bi.Size < uint64(bi.HeaderSize) {
			a.Faults = append(a.Faults, fault.Fault{
				Cat: fault.CatIllegalSize, Byte: int64(bi.Offset), Ref: ref,
				Msg: fmt.Sprintf("size %d smaller than header %d", bi.Size, bi.HeaderSize),
			})
			return a, nil
		}
		if boxEnd > size {
			a.Faults = append(a.Faults, fault.Fault{
				Cat: fault.CatTruncatedBox, Byte: int64(bi.Offset), Ref: ref,
				Msg: fmt.Sprintf("declared end %d exceeds file size %d", boxEnd, size),
			})
			return a, nil
		}
		if bi.Offset != uint64(pos) {
			a.Faults = append(a.Faults, fault.Fault{
				Cat: fault.CatIllegalSize, Byte: int64(bi.Offset), Ref: ref,
				Msg: "box offset does not match contiguous layout",
			})
		}
		a.Boxes = append(a.Boxes, Box{
			Type: ref, Offset: int64(bi.Offset), Size: int64(bi.Size), Depth: 0,
		})
		seen[ref]++
		if ref == "mdat" {
			a.MdatOffsets = append(a.MdatOffsets, int64(bi.Offset+bi.HeaderSize))
		}
		if ref == "moov" {
			fs := a.scanChildren(r, bi)
			a.Faults = append(a.Faults, fs...)
		}
		if _, err := r.Seek(boxEnd, io.SeekStart); err != nil {
			return nil, err
		}
		pos = boxEnd
	}
	for typ := range requiredTopLevel {
		if seen[typ] == 0 {
			a.Faults = append(a.Faults, fault.Fault{
				Cat: fault.CatIllegalSize, Byte: -1, Ref: typ,
				Msg: "required top-level box missing",
			})
		}
	}
	return a, nil
}

// scanChildren validates nested boxes inside moov against the parent end.
func (a *Analysis) scanChildren(r io.ReadSeeker, parent *mp4.BoxInfo) []fault.Fault {
	var fs []fault.Fault
	start := int64(parent.Offset + parent.HeaderSize)
	end := int64(parent.Offset + parent.Size)
	if _, err := r.Seek(start, io.SeekStart); err != nil {
		return []fault.Fault{{Cat: fault.CatTruncatedBox, Byte: start, Ref: parent.Type.String(), Msg: err.Error()}}
	}
	pos := start
	for pos < end {
		bi, err := mp4.ReadBoxInfo(r)
		if err != nil {
			fs = append(fs, fault.Fault{
				Cat: fault.CatTruncatedBox, Byte: pos, Ref: parent.Type.String(),
				Msg: "child box header truncated",
			})
			return fs
		}
		if int64(bi.Offset+bi.Size) > end {
			fs = append(fs, fault.Fault{
				Cat: fault.CatTruncatedBox, Byte: int64(bi.Offset),
				Ref: bi.Type.String(), Msg: "child box exceeds parent boundary",
			})
			return fs
		}
		a.Boxes = append(a.Boxes, Box{
			Type: bi.Type.String(), Offset: int64(bi.Offset),
			Size: int64(bi.Size), Depth: 1,
		})
		pos = int64(bi.Offset + bi.Size)
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			fs = append(fs, fault.Fault{Cat: fault.CatTruncatedBox, Byte: pos, Ref: bi.Type.String(), Msg: err.Error()})
			return fs
		}
	}
	return fs
}

// BuildMinimalMP4 serializes a deterministic ftyp/moov/mdat container whose
// mdat payload is exactly payload. All sizes are explicit 32-bit box sizes.
func BuildMinimalMP4(payload []byte) ([]byte, error) {
	if len(payload) < 0 || int64(len(payload)) > 1<<32-16 {
		return nil, errors.New("mp4: payload size out of range")
	}
	var buf bytes.Buffer
	writeBox := func(typ string, body []byte) {
		var hdr [8]byte
		hdr[0] = byte((8 + len(body)) >> 24)
		hdr[1] = byte((8 + len(body)) >> 16)
		hdr[2] = byte((8 + len(body)) >> 8)
		hdr[3] = byte(8 + len(body))
		copy(hdr[4:], typ)
		buf.Write(hdr[:])
		buf.Write(body)
	}
	writeBox("ftyp", []byte("isom\x00\x00\x02\x00isomiso2"))
	mvhd := make([]byte, 32) // minimal placeholder mvhd body
	writeBox("moov", func() []byte {
		var inner bytes.Buffer
		var hdr [8]byte
		binaryPutBoxHeader(&hdr, "mvhd", 8+len(mvhd))
		inner.Write(hdr[:])
		inner.Write(mvhd)
		return inner.Bytes()
	}())
	writeBox("mdat", payload)
	return buf.Bytes(), nil
}

func binaryPutBoxHeader(hdr *[8]byte, typ string, size int) {
	hdr[0] = byte(size >> 24)
	hdr[1] = byte(size >> 16)
	hdr[2] = byte(size >> 8)
	hdr[3] = byte(size)
	copy(hdr[4:], typ)
}
