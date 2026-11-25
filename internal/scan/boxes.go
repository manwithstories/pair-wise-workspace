// Package scan walks ISO-BMFF / CMAF fragmented MP4 boxes, extracts
// moof/tfdt/trun timing information and sync samples, and records every
// damaged location as a unified fault.Fault instead of aborting at the
// first bad box.
package scan

import (
	"encoding/binary"
	"fmt"
	"io"

	"archivectl/internal/fault"
)

// BoxHeader is a located ISO-BMFF box header.
type BoxHeader struct {
	Type       string
	Offset     int64 // absolute offset of the size field
	HeaderSize int64 // 8 or 16
	Size       int64 // total box size including header; 0 means to EOF
}

// PayloadRange returns the absolute start and declared payload length.
func (h BoxHeader) PayloadRange() (start int64, length int64) {
	return h.Offset + h.HeaderSize, h.Size - h.HeaderSize
}

// readHeader reads one box header at the current seek position. It does
// not move beyond the 8/16 header bytes.
func readHeader(r io.ReadSeeker) (BoxHeader, error) {
	off, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return BoxHeader{}, err
	}
	var small [8]byte
	if _, err := io.ReadFull(r, small[:]); err != nil {
		return BoxHeader{}, fmt.Errorf("read box header at %d: %w", off, err)
	}
	size32 := binary.BigEndian.Uint32(small[0:4])
	typ := string(small[4:8])
	h := BoxHeader{Type: typ, Offset: off, HeaderSize: 8, Size: int64(size32)}
	if size32 == 1 {
		var large [8]byte
		if _, err := io.ReadFull(r, large[:]); err != nil {
			return h, fmt.Errorf("read largesize at %d: %w", off, err)
		}
		h.Size = int64(binary.BigEndian.Uint64(large[:]))
		h.HeaderSize = 16
	}
	return h, nil
}

// fileSize resolves the full size of the seeker without changing the
// caller's logical position permanently.
func fileSize(r io.ReadSeeker) (int64, error) {
	cur, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := r.Seek(cur, io.SeekStart); err != nil {
		return 0, err
	}
	return end, nil
}

// readPayload reads exactly the declared payload of h, recording a
// fatal truncation fault if fewer bytes are available. The returned
// boolean is false when the payload could not be fully read.
func readPayload(r io.ReadSeeker, h BoxHeader, end int64, track string, rec *fault.Record) ([]byte, bool) {
	start, plen := h.PayloadRange()
	if plen < 0 {
		rec.Add(fault.New("box_bad_size", track, h.Offset, fault.LevelFatal,
			fmt.Sprintf("box %q header size %d smaller than header", h.Type, h.Size)))
		return nil, false
	}
	if h.Size == 0 {
		plen = end - start
	}
	if start+plen > end {
		rec.Add(fault.New("box_truncated", track, h.Offset, fault.LevelFatal,
			fmt.Sprintf("box %q declares %d payload bytes ending at %d but file ends at %d",
				h.Type, plen, start+plen, end)))
		// Clamp to the bytes that actually exist so callers can still
		// recover partial trun entries.
		plen = end - start
		if plen < 0 {
			plen = 0
		}
	}
	if _, err := r.Seek(start, io.SeekStart); err != nil {
		rec.Add(fault.New("box_seek_failed", track, start, fault.LevelFatal, err.Error()))
		return nil, false
	}
	buf := make([]byte, plen)
	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		rec.Add(fault.New("box_read_failed", track, start, fault.LevelFatal, err.Error()))
		return nil, false
	}
	return buf[:n], true
}

// walkChildren enumerates direct child boxes inside payload [start,end).
// A damaged child length is reported and traversal stops.
func walkChildren(r io.ReadSeeker, start, end int64, fn func(BoxHeader) error) error {
	pos := start
	for pos+8 <= end {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return err
		}
		h, err := readHeader(r)
		if err != nil {
			return err
		}
		if h.Size < h.HeaderSize && h.Size != 0 {
			return fmt.Errorf("scan: box at %d has invalid size %d", pos, h.Size)
		}
		childEnd := pos + h.Size
		if h.Size == 0 {
			childEnd = end
		}
		if childEnd > end {
			// Let the callback/readPayload record the truncation using
			// the real outer bound; still visit the header.
			childEnd = end
		}
		if err := fn(h); err != nil {
			return err
		}
		if h.Size == 0 {
			break
		}
		pos += h.Size
	}
	return nil
}

// beUint24 decodes a 24-bit big endian integer (tfhd/trun flags share
// the FullBox 3-byte flag field).
func beUint24(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}
