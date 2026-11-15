// Package box implements self-contained ISO BMFF (MP4) box header parsing.
// Only box type constants are borrowed from github.com/abema/go-mp4; all
// header traversal is implemented here and shared by the indexer and the
// exporter so they never parse the container twice.
package box

import (
	"errors"
	"fmt"

	mp4 "github.com/abema/go-mp4"
)

// Limits and sizes used by the reader.
const (
	// MaxDepth bounds container nesting (moov/trak/mdia/minf/stbl is 5).
	MaxDepth = 8
	// SmallHeaderSize is size32+type.
	SmallHeaderSize = 8
	// LargeHeaderSize adds a 64-bit largesize field.
	LargeHeaderSize = 16
	// UUIDExtra is the usertype extension of uuid boxes.
	UUIDExtra = 16
	// MaxPayload guards against absurd in-memory table loads (256 MiB).
	MaxPayload = 1 << 28
)

// ErrStructure marks every box-level structural violation. The CLI maps
// errors wrapping it to exit code 3.
var ErrStructure = errors.New("box structure error")

// Error describes one structural problem at a file offset.
type Error struct {
	Op     string
	Offset uint64
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s at offset %d: %s", e.Op, e.Offset, e.Msg)
}

// Unwrap lets errors.Is(err, box.ErrStructure) succeed.
func (e *Error) Unwrap() error { return ErrStructure }

func errf(op string, off uint64, format string, args ...interface{}) *Error {
	return &Error{Op: op, Offset: off, Msg: fmt.Sprintf(format, args...)}
}

// Errorf builds a structural error; used by higher layers (moov tables)
// so every container violation maps to the same exit code.
func Errorf(op string, off uint64, format string, args ...interface{}) *Error {
	return errf(op, off, format, args...)
}

// ErrTruncated reports a box extending past its parent or the file end.
func ErrTruncated(op string, off uint64, want, have uint64) *Error {
	return errf(op, off, "truncated: need %d bytes, %d remain", want, have)
}

// ErrMissingBox reports a required box that was not found.
func ErrMissingBox(op, typ string) *Error {
	return &Error{Op: op, Offset: 0, Msg: "required box missing: " + typ}
}

// Header describes one parsed box header.
type Header struct {
	Type       string // 4cc, or "uuid" (usertype skipped)
	Offset     uint64 // absolute offset of the box header
	Size       uint64 // total box size including header
	HeaderSize uint64 // 8, 16, 24 or 32 (uuid variants)
}

// Payload returns the box payload size in bytes.
func (h Header) Payload() uint64 { return h.Size - h.HeaderSize }

// DataOffset returns the absolute offset of the payload.
func (h Header) DataOffset() uint64 { return h.Offset + h.HeaderSize }

// End returns the absolute offset one past the box.
func (h Header) End() uint64 { return h.Offset + h.Size }

// fourcc converts a go-mp4 box type constant to its string form.
func fourcc(t mp4.BoxType) string { return string(t[:]) }

// containers lists box types whose payload is a sequence of child boxes.
var containers = map[string]bool{
	fourcc(mp4.BoxTypeMoov()): true, fourcc(mp4.BoxTypeTrak()): true,
	fourcc(mp4.BoxTypeEdts()): true, fourcc(mp4.BoxTypeMdia()): true,
	fourcc(mp4.BoxTypeMinf()): true, fourcc(mp4.BoxTypeDinf()): true,
	fourcc(mp4.BoxTypeStbl()): true, fourcc(mp4.BoxTypeUdta()): true,
	fourcc(mp4.BoxTypeMoof()): true, fourcc(mp4.BoxTypeTraf()): true,
	fourcc(mp4.BoxTypeMvex()): true, fourcc(mp4.BoxTypeMfra()): true,
}

// IsContainer reports whether typ is a known container box.
func IsContainer(typ string) bool { return containers[typ] }

// known lists every fourcc we recognise; anything else is skipped and
// counted as unknown by the walker. Constants come from go-mp4; the
// handful it does not define are string literals.
var known = map[string]bool{
	fourcc(mp4.BoxTypeFtyp()): true, fourcc(mp4.BoxTypeStyp()): true,
	fourcc(mp4.BoxTypeMoov()): true, fourcc(mp4.BoxTypeMvhd()): true,
	fourcc(mp4.BoxTypeTrak()): true, fourcc(mp4.BoxTypeTkhd()): true,
	fourcc(mp4.BoxTypeEdts()): true, fourcc(mp4.BoxTypeElst()): true,
	fourcc(mp4.BoxTypeMdia()): true, fourcc(mp4.BoxTypeMdhd()): true,
	fourcc(mp4.BoxTypeHdlr()): true, fourcc(mp4.BoxTypeMinf()): true,
	fourcc(mp4.BoxTypeVmhd()): true, fourcc(mp4.BoxTypeSmhd()): true,
	"hmhd": true, "nmhd": true,
	fourcc(mp4.BoxTypeDinf()): true, fourcc(mp4.BoxTypeDref()): true,
	fourcc(mp4.BoxTypeUrl()): true, fourcc(mp4.BoxTypeUrn()): true,
	fourcc(mp4.BoxTypeStbl()): true, fourcc(mp4.BoxTypeStsd()): true,
	fourcc(mp4.BoxTypeStts()): true, fourcc(mp4.BoxTypeCtts()): true,
	fourcc(mp4.BoxTypeStss()): true, fourcc(mp4.BoxTypeStsc()): true,
	fourcc(mp4.BoxTypeStsz()): true, "stz2": true,
	fourcc(mp4.BoxTypeStco()): true, fourcc(mp4.BoxTypeCo64()): true,
	fourcc(mp4.BoxTypeUdta()): true, fourcc(mp4.BoxTypeMdat()): true,
	fourcc(mp4.BoxTypeFree()): true, fourcc(mp4.BoxTypeSkip()): true,
	"wide": true, TypeUUID: true,
	fourcc(mp4.BoxTypeMoof()): true, fourcc(mp4.BoxTypeMfhd()): true,
	fourcc(mp4.BoxTypeTraf()): true, fourcc(mp4.BoxTypeMvex()): true,
	fourcc(mp4.BoxTypeMfra()): true, fourcc(mp4.BoxTypeSidx()): true,
	"ssix": true, "prft": true,
}

// TypeUUID is the uuid box fourcc (go-mp4 v1.2.0 defines no constant).
const TypeUUID = "uuid"

// IsKnown reports whether typ is a recognised fourcc.
func IsKnown(typ string) bool { return known[typ] }
