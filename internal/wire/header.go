package wire

import (
	"encoding/binary"

	"relayaudit/internal/fault"
)

// Header is the decoded segment header.
type Header struct {
	Version     uint8
	Tracks      uint8
	RecordCount uint32
	PageCount   uint32
	DataStart   int64
}

// DefaultTracks is the track count of the reference fixture.
const DefaultTracks = 4

// EncodeHeader writes hdr into a 24-byte array.
func EncodeHeader(h Header) [HeaderSize]byte {
	var b [HeaderSize]byte
	copy(b[0:4], Magic)
	b[4] = h.Version
	b[5] = h.Tracks
	binary.BigEndian.PutUint32(b[8:12], h.RecordCount)
	binary.BigEndian.PutUint32(b[12:16], h.PageCount)
	binary.BigEndian.PutUint64(b[16:24], uint64(h.DataStart))
	return b
}

// ParseHeader validates the segment header of buf.
func ParseHeader(buf []byte) (Header, error) {
	var h Header
	if len(buf) < HeaderSize {
		return h, fault.New(fault.Truncated, 0, "segment header needs %d bytes, have %d", HeaderSize, len(buf))
	}
	if string(buf[0:4]) != Magic {
		return h, fault.New(fault.Length, 0, "bad segment magic %q", buf[0:4])
	}
	if buf[4] != Version {
		return h, fault.New(fault.Length, 4, "unsupported format version %d", buf[4])
	}
	h.Version = buf[4]
	h.Tracks = buf[5]
	if h.Tracks == 0 || h.Tracks > MaxTracks {
		return h, fault.New(fault.Limit, 5, "implausible track count %d", h.Tracks)
	}
	h.RecordCount = binary.BigEndian.Uint32(buf[8:12])
	h.PageCount = binary.BigEndian.Uint32(buf[12:16])
	if h.PageCount > MaxIndexPages {
		return h, fault.New(fault.Limit, 12, "index page count %d exceeds limit %d", h.PageCount, MaxIndexPages)
	}
	ds := binary.BigEndian.Uint64(buf[16:24])
	if ds < HeaderSize {
		return h, fault.New(fault.Index, 16, "data start %d precedes header end %d", ds, HeaderSize)
	}
	h.DataStart = int64(ds)
	// The index region is validated by ParseIndex against both the declared
	// page count and the data start; keeping that check out of header parsing
	// lets Scan rebuild an index whose page region is short or overlapping.
	return h, nil
}

// ValidateBounds checks the header against the actual file length. It is
// kept separate from ParseHeader so that a header can be inspected before
// the rest of the file is available. Only the header and the declared data
// start must be present; a short index region is reported by ParseIndex and
// triggers a rebuild instead of failing the segment outright.
func (h Header) ValidateBounds(size int64) error {
	if h.DataStart > size {
		return fault.New(fault.Index, 16, "data start %d outside file of %d bytes", h.DataStart, size)
	}
	return nil
}
