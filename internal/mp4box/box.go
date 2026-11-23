// Package mp4box provides low-level ISO BMFF box scanning with exact byte
// offset accounting. It wraps github.com/abema/go-mp4 for box type constants
// and payload (un)marshalling while keeping structural walking under our own
// control so that truncated/corrupt inputs can be reported with the first
// confirmable abnormal byte.
package mp4box

import (
	"encoding/binary"
	"io"

	"github.com/abema/go-mp4"
	"mediacore/internal/fault"
)

// Header is a decoded ISO BMFF box header.
type Header struct {
	Type       mp4.BoxType
	Offset     int64 // first byte of the box (size field)
	HeaderSize int64 // 8 (small) or 16 (largesize)
	Size       int64 // total declared box size including header; -1 if extends to EOF
	PayloadOff int64 // first payload byte
	End        int64 // one past declared end; -1 if extends to EOF
	// DeclaredEnd always retains the box's own declared end (offset+size),
	// even when End is clipped to -1 for running past the file boundary.
	DeclaredEnd int64
}

// ReadHeader reads exactly one box header at the current seek position.
// A truncated header is reported as KindTruncatedHeader carrying the offset of
// the first missing byte.
func ReadHeader(r io.ReadSeeker) (*Header, error) {
	off, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	var hdr [8]byte
	n, err := io.ReadFull(r, hdr[:])
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		if n == 0 {
			return nil, io.EOF // clean end of container
		}
		return nil, fault.New(fault.KindTruncatedHeader, fault.Abort,
			off+int64(n), "box header truncated before 8 bytes")
	}
	if err != nil {
		return nil, err
	}
	h := &Header{Offset: off, HeaderSize: 8}
	copy(h.Type[:], hdr[4:8])
	size := int64(binary.BigEndian.Uint32(hdr[:4]))
	if size == 1 {
		var ext [8]byte
		n2, err := io.ReadFull(r, ext[:])
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, fault.New(fault.KindTruncatedHeader, fault.Abort,
				off+8+int64(n2), "largesize field truncated")
		}
		if err != nil {
			return nil, err
		}
		h.HeaderSize = 16
		size = int64(binary.BigEndian.Uint64(ext[:]))
	}
	if size == 0 {
		h.Size, h.End, h.DeclaredEnd = -1, -1, -1 // box extends to EOF
	} else {
		if size < h.HeaderSize {
			return nil, fault.New(fault.KindTruncatedHeader, fault.Abort, off,
				"declared box size smaller than its header")
		}
		h.Size = size
		h.End = off + size
		h.DeclaredEnd = h.End
	}
	h.PayloadOff = off + h.HeaderSize
	return h, nil
}

// Children scans the immediate child boxes of the container whose header is
// given. The seeker is left at an unspecified position. limit bounds the file
// region that may be read.
func Children(r io.ReadSeeker, parent *Header, limit int64) ([]*Header, error) {
	if _, err := r.Seek(parent.PayloadOff, io.SeekStart); err != nil {
		return nil, err
	}
	end := parent.End
	if end < 0 || (limit > 0 && (end < 0 || end > limit)) {
		end = limit
	}
	var out []*Header
	for {
		pos, err := r.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		if end >= 0 && pos >= end {
			return out, nil
		}
		h, err := ReadHeader(r)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		if h.End >= 0 && end >= 0 && h.End > end {
			if parent.Offset == 0 && parent.HeaderSize == 0 {
				overrun := h.End - end
				if overrun >= 8 {
					// The box itself is truncated mid-payload (e.g. a cut mdat).
					// Keep its header; the fragment is reported per sample as a
					// recoverable fault.
					h.End = -1 // mark as EOF-extended for downstream bounds
					out = append(out, h)
					return out, nil
				}
				// A sub-8-byte overrun means the box payload is intact but a
				// dangling half header follows: structural truncation, abort.
				return out, fault.New(fault.KindTruncatedHeader, fault.Abort, end,
					"dangling partial box header at end of file")
			}
			return out, fault.New(fault.KindTruncatedHeader, fault.Abort, h.Offset,
				"child box extends past its container end")
		}
		out = append(out, h)
		if h.End < 0 {
			return out, nil
		}
		if _, err := r.Seek(h.End, io.SeekStart); err != nil {
			return nil, err
		}
	}
}

// Find returns the first child with the given type, or nil.
func Find(hs []*Header, typ mp4.BoxType) *Header {
	for _, h := range hs {
		if h.Type == typ {
			return h
		}
	}
	return nil
}

// TailFragment reports a dangling partial box header at the end of the file:
// bytes that remain after every complete box but cannot form an 8-byte header.
// It returns nil when the file ends cleanly or when the last complete box is
// itself EOF-extended (its trailing bytes are payload, not a new box).
func TailFragment(r io.ReadSeeker, tops []*Header, fileSize int64) *fault.Fault {
	var pos int64
	for _, h := range tops {
		// Use the box's own declared end. A truncated mdat declares past EOF,
		// so its declared end >= fileSize and leaves no residual region. A
		// complete mdat followed by a half header leaves a real gap.
		end := h.DeclaredEnd
		if end < 0 {
			end = fileSize
		}
		if end > pos {
			pos = end
		}
	}
	if pos >= fileSize {
		return nil
	}
	// Residual bytes after the last fully-declared box indicate a structural
	// truncation (a half box header).
	if _, err := r.Seek(pos, io.SeekStart); err != nil {
		return fault.New(fault.KindTruncatedHeader, fault.Abort, pos, err.Error())
	}
	return fault.New(fault.KindTruncatedHeader, fault.Abort, pos,
		"dangling partial box header after last complete box")
}

// Payload reads the full payload of a box, capped at maxBytes.
func Payload(r io.ReadSeeker, h *Header, maxBytes int) ([]byte, error) {
	if h.Size < 0 {
		return nil, fault.New(fault.KindTruncatedHeader, fault.Abort, h.Offset,
			"cannot read payload of EOF-extended box")
	}
	plen := int(h.Size - h.HeaderSize)
	if plen > maxBytes {
		return nil, fault.New(fault.KindLimitExceeded, fault.Abort, h.PayloadOff,
			"box payload exceeds configured size limit")
	}
	buf := make([]byte, plen)
	if _, err := r.Seek(h.PayloadOff, io.SeekStart); err != nil {
		return nil, err
	}
	n, err := io.ReadFull(r, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return nil, fault.New(fault.KindTruncatedHeader, fault.Abort,
			h.PayloadOff+int64(n), "box payload truncated")
	}
	return buf, err
}
