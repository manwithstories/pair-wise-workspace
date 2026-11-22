package ingest

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"mediarun/internal/mediaerr"
)

// boxHeader is the validated boundary of one ISOBMFF box. Payload ranges
// over [PayloadStart, PayloadStart+PayloadLen) and the whole box (header
// included) ends at End.
type boxHeader struct {
	Type         mp4.BoxType
	HeaderStart  int64 // absolute offset of the size field
	PayloadStart int64 // absolute offset of the first payload byte
	PayloadLen   int64
	End          int64
}

// boxWalker performs a streaming walk over MP4 box boundaries. Every length
// decision is checked against the supplied file size so a truncated box or
// malformed length becomes a TRUNCATED_BOX error carrying the exact input
// offset instead of a short read deep inside a payload parser.
type boxWalker struct {
	r        *bufio.Reader
	fileSize int64
	pos      int64
}

func newBoxWalker(r io.Reader, fileSize int64) *boxWalker {
	return &boxWalker{r: bufio.NewReaderSize(r, 64*1024), fileSize: fileSize}
}

// next reads the next box header at the current position, honoring 64-bit
// largesize and rejecting sub-header or past-EOF sizes.
func (w *boxWalker) next() (*boxHeader, error) {
	start := w.pos
	if start >= w.fileSize {
		return nil, io.EOF
	}
	if w.fileSize-start < 8 {
		return nil, mediaerr.New(mediaerr.CodeTruncatedBox, fmt.Sprintf(
			"only %d bytes left for a box header (need 8)", w.fileSize-start)).
			At("mp4", "", start)
	}
	head := make([]byte, 8)
	if _, err := io.ReadFull(w.r, head); err != nil {
		return nil, mediaerr.New(mediaerr.CodeTruncatedBox, err.Error()).
			At("mp4", "", start)
	}
	size32 := int64(binary.BigEndian.Uint32(head[:4]))
	btype := mp4.BoxType{head[4], head[5], head[6], head[7]}
	headerLen := int64(8)
	size := size32
	switch size32 {
	case 0: // box extends to end of file
		size = w.fileSize - start
	case 1: // 64-bit largesize follows
		ext := make([]byte, 8)
		if _, err := io.ReadFull(w.r, ext); err != nil {
			return nil, mediaerr.New(mediaerr.CodeTruncatedBox,
				"truncated 64-bit largesize").At("mp4", "", start)
		}
		headerLen = 16
		size = int64(binary.BigEndian.Uint64(ext))
	}
	if size < headerLen {
		return nil, mediaerr.New(mediaerr.CodeTruncatedBox, fmt.Sprintf(
			"box '%s' size %d smaller than header %d", btype.String(), size, headerLen)).
			At("mp4", "", start)
	}
	end := start + size
	if end > w.fileSize {
		return nil, mediaerr.New(mediaerr.CodeTruncatedBox, fmt.Sprintf(
			"box '%s' declares end %d past EOF %d", btype.String(), end, w.fileSize)).
			At("mp4", "", start)
	}
	w.pos = end
	return &boxHeader{
		Type:         btype,
		HeaderStart:  start,
		PayloadStart: start + headerLen,
		PayloadLen:   size - headerLen,
		End:          end,
	}, nil
}

// parseBox decodes one box payload into the go-mp4 typed struct so field
// semantics stay identical to the reference library. Unknown box types are
// surfaced as UNKNOWN_STRUCTURE rather than silently skipped.
func parseBox(r io.ReadSeeker, b *boxHeader) (interface{}, error) {
	if _, err := r.Seek(b.PayloadStart, io.SeekStart); err != nil {
		return nil, mediaerr.New(mediaerr.CodeIO, err.Error()).At("mp4", "", b.PayloadStart)
	}
	val, _, err := mp4.UnmarshalAny(r, b.Type, uint64(b.PayloadLen), mp4.Context{})
	if err != nil {
		if err == mp4.ErrBoxInfoNotFound {
			return nil, mediaerr.New(mediaerr.CodeUnknownStructure, fmt.Sprintf(
				"box type '%s' is not in the documented subset", b.Type.String())).
				At("mp4", "", b.HeaderStart)
		}
		return nil, mediaerr.New(mediaerr.CodeTruncatedBox, err.Error()).
			At("mp4", "", b.PayloadStart)
	}
	return val, nil
}

// findChild walks the direct children of container box b, invoking fn for
// each child matching want. Parsing stops when fn returns true.
func findChild(r io.ReadSeeker, b *boxHeader, want mp4.BoxType,
	fn func(child *boxHeader) (bool, error)) error {
	if _, err := r.Seek(b.PayloadStart, io.SeekStart); err != nil {
		return mediaerr.New(mediaerr.CodeIO, err.Error()).At("mp4", "", b.PayloadStart)
	}
	w := &boxWalker{r: bufio.NewReader(r), fileSize: b.End, pos: b.PayloadStart}
	for w.pos < b.End {
		child, err := w.next()
		if err != nil {
			return err
		}
		if boxTypeEqual(child.Type, want) {
			stop, err := fn(child)
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
		}
	}
	return nil
}

// boxTypeEqual hides the 4-byte array comparison noise.
func boxTypeEqual(a, b mp4.BoxType) bool { return bytes.Equal(a[:], b[:]) }

// onPath attaches an input path to a structured error; non-structured errors
// are wrapped as IO_FAILURE so the location is never lost.
func onPath(err error, path string) error {
	if err == nil {
		return nil
	}
	if se, ok := err.(*mediaerr.Error); ok {
		return se.OnPath(path)
	}
	return mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(path)
}
