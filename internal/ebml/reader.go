// Package ebml parses the Matroska/WebM EBML container structure,
// including elements with unknown (streaming) sizes, and reports
// structured errors carrying byte offset, track number and a stable
// error code. A parse error always aborts the document: callers must
// not emit a success manifest for rejected input.
package ebml

import (
	"encoding/binary"
	"fmt"
)

// Element IDs interpreted by this parser.
const (
	IDSegment       uint64 = 0x1A45DFA3
	IDInfo          uint64 = 0x1549A966
	IDTimeScale     uint64 = 0x2AD7B1
	IDTracks        uint64 = 0x1654AE6B
	IDCluster       uint64 = 0x1F43B675
	IDTimestamp     uint64 = 0xE7
	IDBlockGroup    uint64 = 0xA0
	IDBlock         uint64 = 0xA1
	IDSimpleBlock   uint64 = 0xA3
	IDBlockDuration uint64 = 0x9B
)

// ErrorCode is a stable machine-readable rejection reason.
type ErrorCode string

const (
	ErrTruncatedVINT ErrorCode = "EBML_TRUNCATED_VINT"
	ErrSizeOverrun   ErrorCode = "EBML_SIZE_OVERRUN"
	ErrBadElement    ErrorCode = "EBML_BAD_ELEMENT"
)

// Error describes a rejected EBML structure.
type Error struct {
	Code   ErrorCode
	Offset int64
	Track  uint64 // 0 when no track context exists yet
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s offset=%d track=%d: %s", e.Code, e.Offset, e.Track, e.Msg)
}

// Block is one parsed Block/SimpleBlock payload.
type Block struct {
	Track     uint64
	Timestamp int64 // absolute, in TimeScale ticks
	Duration  uint64 // ticks; 0 when undeclared
	Data      []byte
	Offset    int64
}

// Document is the parsed container.
type Document struct {
	TimeScale uint64 // nanoseconds per tick (Matroska default 1_000_000)
	Blocks    []Block
}

// Parse walks the whole buffer and returns the document, or the first
// structured error encountered.
func Parse(data []byte) (*Document, error) {
	p := &parser{data: data}
	st := &state{doc: &Document{TimeScale: 1_000_000}, clusterBase: new(int64), last: new(int)}
	*st.last = -1
	if err := p.walk(0, int64(len(data)), st, nil); err != nil {
		return nil, err
	}
	return st.doc, nil
}

type state struct {
	doc         *Document
	clusterBase *int64
	last        *int // index of last block, for BlockDuration attach
}

type parser struct {
	data []byte
	pos  int64
}

// walk parses elements in [start, end). stopAt lists element IDs that
// terminate the current master element (used to bound unknown-size
// clusters at the next cluster boundary).
func (p *parser) walk(start, end int64, st *state, stopAt map[uint64]bool) error {
	p.pos = start
	for p.pos < end {
		elemStart := p.pos
		id, err := p.readID()
		if err != nil {
			return err
		}
		if stopAt[id] {
			p.pos = elemStart
			return nil
		}
		size, unknown, err := p.readSize()
		if err != nil {
			return err
		}
		payloadStart := p.pos
		payloadEnd := end
		if !unknown {
			payloadEnd = payloadStart + int64(size)
			if payloadEnd > end {
				return &Error{Code: ErrSizeOverrun, Offset: elemStart,
					Msg: fmt.Sprintf("element 0x%x declares %d bytes beyond parent end", id, size)}
			}
		} else if id != IDSegment && id != IDCluster {
			return &Error{Code: ErrBadElement, Offset: elemStart,
				Msg: fmt.Sprintf("unknown size on non-master element 0x%x", id)}
		}
		switch id {
		case IDSegment, IDInfo, IDTracks, IDBlockGroup:
			if err := p.walk(payloadStart, payloadEnd, st, nil); err != nil {
				return err
			}
		case IDCluster:
			child := &state{doc: st.doc, clusterBase: new(int64), last: new(int)}
			*child.last = -1
			if err := p.walk(payloadStart, payloadEnd, child, map[uint64]bool{IDCluster: true}); err != nil {
				return err
			}
		case IDTimestamp:
			v, err := p.readUint(payloadStart, payloadEnd)
			if err != nil {
				return err
			}
			*st.clusterBase = int64(v)
		case IDTimeScale:
			v, err := p.readUint(payloadStart, payloadEnd)
			if err != nil {
				return err
			}
			st.doc.TimeScale = v
		case IDBlock, IDSimpleBlock:
			if err := p.parseBlock(elemStart, payloadStart, payloadEnd, st); err != nil {
				return err
			}
		case IDBlockDuration:
			v, err := p.readUint(payloadStart, payloadEnd)
			if err != nil {
				return err
			}
			if *st.last >= 0 {
				st.doc.Blocks[*st.last].Duration = v
			}
		}
		if unknown {
			// The child walk left p.pos at the next sibling.
		} else {
			p.pos = payloadEnd
		}
	}
	return nil
}

func (p *parser) readID() (uint64, error) {
	v, _, err := p.readVINT()
	return v, err
}

func (p *parser) readSize() (uint64, bool, error) {
	v, width, err := p.readVINT()
	if err != nil {
		return 0, false, err
	}
	mask := uint64(1) << uint(7*width)
	data := v &^ mask
	if data == mask-1 {
		return 0, true, nil
	}
	return data, false, nil
}

// readVINT reads a variable-length integer keeping the marker bit.
func (p *parser) readVINT() (uint64, int, error) {
	start := p.pos
	if p.pos >= int64(len(p.data)) {
		return 0, 0, &Error{Code: ErrTruncatedVINT, Offset: start, Msg: "unexpected end of data reading VINT"}
	}
	b := p.data[p.pos]
	if b == 0 {
		return 0, 0, &Error{Code: ErrBadElement, Offset: start, Msg: "VINT with zero leading byte"}
	}
	width := 1
	for mask := byte(0x80); b&mask == 0; mask >>= 1 {
		width++
	}
	remain := int64(len(p.data)) - p.pos
	if int64(width) > remain {
		return 0, 0, &Error{Code: ErrTruncatedVINT, Offset: start,
			Msg: fmt.Sprintf("VINT needs %d bytes, only %d remain", width, remain)}
	}
	var v uint64
	for i := 0; i < width; i++ {
		v = v<<8 | uint64(p.data[p.pos+int64(i)])
	}
	p.pos += int64(width)
	return v, width, nil
}

func (p *parser) readUint(start, end int64) (uint64, error) {
	n := end - start
	if n < 1 || n > 8 {
		return 0, &Error{Code: ErrBadElement, Offset: start, Msg: fmt.Sprintf("uint element of %d bytes", n)}
	}
	var v uint64
	for i := start; i < end; i++ {
		v = v<<8 | uint64(p.data[i])
	}
	return v, nil
}

func (p *parser) parseBlock(elemStart, start, end int64, st *state) error {
	p.pos = start
	tv, width, err := p.readVINT()
	if err != nil {
		return err
	}
	track := tv &^ (uint64(1) << uint(7*width))
	if p.pos+3 > end {
		return &Error{Code: ErrTruncatedVINT, Offset: p.pos, Track: track, Msg: "truncated block header"}
	}
	ts := int64(int16(binary.BigEndian.Uint16(p.data[p.pos:])))
	flags := p.data[p.pos+2]
	if flags&0x06 != 0 {
		return &Error{Code: ErrBadElement, Offset: elemStart, Track: track, Msg: "laced blocks are unsupported"}
	}
	blk := Block{
		Track:     track,
		Timestamp: *st.clusterBase + ts,
		Data:      p.data[p.pos+3 : end],
		Offset:    elemStart,
	}
	st.doc.Blocks = append(st.doc.Blocks, blk)
	*st.last = len(st.doc.Blocks) - 1
	return nil
}
