package ingest

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/abema/go-mp4"
)

// The mediad container is a stream of ISO-BMFF boxes:
//
//	ftyp                     major brand "mdia"
//	mtrk                     one per file: track declaration
//	mdfr                     one per frame, in source order
//
// Keeping the declaration in a top-level box and frames at fixed boundaries
// means a random-access index can be rebuilt by a single sequential scan, and
// an interrupted writer can append frames without rewriting earlier ones.
// Nothing here relies on a moov index, so a half-written file is still
// parseable up to its last complete frame.

var (
	// TypeTrack is the box type of the track declaration.
	TypeTrack = mp4.StrToBoxType("mtrk")
	// TypeFrame is the box type of a frame record.
	TypeFrame = mp4.StrToBoxType("mdfr")
)

const (
	// HeaderSize is the size of a box header.
	HeaderSize = 8
	// Magic is written into the ftyp major brand of every container.
	Magic = "mdia"
)

func init() {
	mp4.AddAnyTypeBoxDef(&TrackBox{}, TypeTrack, 0)
	mp4.AddAnyTypeBoxDef(&FrameBox{}, TypeFrame, 0)
}

// TrackBox declares one track's identity, geometry and capture clock.
type TrackBox struct {
	mp4.AnyTypeBox
	TrackID    uint32  `mp4:"0,size=32"`
	Width      uint32  `mp4:"1,size=32"`
	Height     uint32  `mp4:"2,size=32"`
	CaptureUPS uint32  `mp4:"3,size=32"`
	Timescale  uint32  `mp4:"4,size=32"`
	EpochUnits uint64  `mp4:"5,size=64"`
	WrapUnits  uint64  `mp4:"6,size=64"`
	TargetKbps uint32  `mp4:"7,size=32"`
	Color      [4]byte `mp4:"8,size=8"`
	FrameCount uint32  `mp4:"9,size=32"`
	Reserved   uint32  `mp4:"10,size=32,const=0"`
}

// FrameBox is one frame record. DeclaredHash precedes the payload so that a
// tail-truncated frame is still identifiable: PayloadLen records the declared
// payload size and is compared with what the box actually carries.
type FrameBox struct {
	mp4.AnyTypeBox
	TrackID      uint32   `mp4:"0,size=32"`
	FrameNo      uint64   `mp4:"1,size=64"`
	CaptureUnits uint64   `mp4:"2,size=64"`
	Width        uint32   `mp4:"3,size=32"`
	Height       uint32   `mp4:"4,size=32"`
	CaptureUPS   uint32   `mp4:"5,size=32"`
	Timescale    uint32   `mp4:"6,size=32"`
	Color        [4]byte  `mp4:"7,size=8"`
	DeclaredHash [32]byte `mp4:"8,size=8"`
	PayloadLen   uint32   `mp4:"9,size=32"`
	PTS          int64    `mp4:"10,size=64"`
	Payload      []byte   `mp4:"11,size=8"`
}

// ErrTruncated reports a container that ends inside a box, or a frame whose
// payload is shorter than its declared length.
var ErrTruncated = errors.New("ingest: truncated container")

// NewTrackBox returns a track box of the correct type.
func NewTrackBox() *TrackBox {
	b := &TrackBox{}
	b.Type = TypeTrack
	return b
}

// NewFrameBox returns a frame box of the correct type.
func NewFrameBox() *FrameBox {
	b := &FrameBox{}
	b.Type = TypeFrame
	return b
}

// WriteBox writes one box (header plus payload) at the current offset and
// returns the offset the box started at.
func WriteBox(w io.WriteSeeker, box mp4.IBox) (offset int64, size int64, err error) {
	offset, err = w.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, 0, err
	}
	var payload bytes.Buffer
	if _, err := mp4.Marshal(&payload, box, mp4.Context{}); err != nil {
		return 0, 0, err
	}
	bi := &mp4.BoxInfo{
		Size:       uint64(HeaderSize + payload.Len()),
		HeaderSize: HeaderSize,
		Type:       box.GetType(),
	}
	if _, err := mp4.WriteBoxInfo(w, bi); err != nil {
		return 0, 0, err
	}
	if _, err := w.Write(payload.Bytes()); err != nil {
		return 0, 0, err
	}
	return offset, int64(HeaderSize + payload.Len()), nil
}

// WriteFtyp writes the leading file-type box.
func WriteFtyp(w io.WriteSeeker) error {
	ftyp := &mp4.Ftyp{MajorBrand: [4]byte{Magic[0], Magic[1], Magic[2], Magic[3]}}
	ftyp.MinorVersion = 0
	ftyp.AddCompatibleBrand([4]byte{Magic[0], Magic[1], Magic[2], Magic[3]})
	_, _, err := WriteBox(w, ftyp)
	return err
}

// ReadFrameAt seeks to offset and decodes the frame box there. It reads only
// the box payload, so caller memory is bounded by one frame.
func ReadFrameAt(r io.ReadSeeker, offset int64, size uint64) (*FrameBox, error) {
	if size < HeaderSize {
		return nil, fmt.Errorf("%w: frame box size %d", ErrTruncated, size)
	}
	if _, err := r.Seek(offset+HeaderSize, io.SeekStart); err != nil {
		return nil, err
	}
	box, _, err := mp4.UnmarshalAny(r, TypeFrame, size-HeaderSize, mp4.Context{})
	if err != nil {
		return nil, err
	}
	fb, ok := box.(*FrameBox)
	if !ok {
		return nil, fmt.Errorf("ingest: frame box decoded as %T", box)
	}
	return fb, nil
}

// ReadTrackAt decodes a track declaration box.
func ReadTrackAt(r io.ReadSeeker, offset int64, size uint64) (*TrackBox, error) {
	if size < HeaderSize {
		return nil, fmt.Errorf("%w: track box size %d", ErrTruncated, size)
	}
	if _, err := r.Seek(offset+HeaderSize, io.SeekStart); err != nil {
		return nil, err
	}
	box, _, err := mp4.UnmarshalAny(r, TypeTrack, size-HeaderSize, mp4.Context{})
	if err != nil {
		return nil, err
	}
	tb, ok := box.(*TrackBox)
	if !ok {
		return nil, fmt.Errorf("ingest: track box decoded as %T", box)
	}
	return tb, nil
}

// PeekHeader reads a box header at offset without moving the payload cursor.
// It reports the declared end offset and whether the box extends past fileEnd,
// which is how a truncated tail is detected before attempting to decode.
func PeekHeader(r io.ReadSeeker, offset, fileEnd int64) (bi *mp4.BoxInfo, truncated bool, err error) {
	if _, err := r.Seek(offset, io.SeekStart); err != nil {
		return nil, false, err
	}
	bi, err = mp4.ReadBoxInfo(r)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, true, nil
		}
		return nil, false, err
	}
	if bi.Offset+bi.Size > uint64(fileEnd) {
		return bi, true, nil
	}
	return bi, false, nil
}
