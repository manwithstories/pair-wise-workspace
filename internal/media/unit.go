// Package media parses coded access-unit headers without retaining media payloads.
package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"medcheck/fault"
)

const (
	// MaxInputBytes is the accepted capture size: 2 GiB.
	MaxInputBytes int64 = 2 << 30
	// Magic marks an medcheck access unit.
	Magic = "MCAU"
	// HeaderSize is the fixed bounded header size.
	HeaderSize = 40
	// Version is the supported header version.
	Version byte = 1
	// KeyFrameFlag marks an IDR/key access unit.
	KeyFrameFlag byte = 1 << 0
)

var (
	ErrTruncatedHeader = errors.New("media: truncated access-unit header")
	ErrBadMagic        = errors.New("media: bad access-unit magic")
	ErrBadCRC          = errors.New("media: header CRC mismatch")
	ErrUnsupportedVer  = errors.New("media: unsupported header version")
)

// AccessUnit is header metadata only; payload bytes are never interpreted.
type AccessUnit struct {
	Offset     int64  `json:"offset"`
	Index      uint32 `json:"index"`
	PTSMS      int64  `json:"pts_ms"`
	GOPID      uint32 `json:"gop_id"`
	IndexInGOP uint16 `json:"index_in_gop"`
	PayloadLen uint32 `json:"payload_len"`
	KeyFrame   bool   `json:"key_frame"`
	CRC        uint32 `json:"crc"`
	CRCValid   bool   `json:"crc_valid"`
}

// HeaderError localizes a malformed or invalid header.
type HeaderError struct {
	Offset int64
	Kind   fault.Kind
	Err    error
}

func (e HeaderError) Error() string {
	return fmt.Sprintf("offset=%d %s: %v", e.Offset, e.Kind, e.Err)
}
func (e HeaderError) Unwrap() error { return e.Err }

// EncodeHeader serializes u into a fixed 40-byte header.
func EncodeHeader(u AccessUnit) []byte {
	h := make([]byte, HeaderSize)
	copy(h[0:4], Magic)
	h[4] = Version
	if u.KeyFrame {
		h[5] |= KeyFrameFlag
	}
	binary.LittleEndian.PutUint16(h[6:8], u.IndexInGOP)
	binary.LittleEndian.PutUint32(h[8:12], u.Index)
	binary.LittleEndian.PutUint32(h[12:16], u.GOPID)
	binary.LittleEndian.PutUint32(h[16:20], u.PayloadLen)
	binary.LittleEndian.PutUint64(h[20:28], uint64(u.PTSMS))
	binary.LittleEndian.PutUint16(h[28:30], u.IndexInGOP)
	crc := crc32.ChecksumIEEE(h[:32])
	binary.LittleEndian.PutUint32(h[32:36], crc)
	return h
}

// DecodeHeader parses exactly HeaderSize bytes. A CRC mismatch returns both
// the best-effort header and ErrBadCRC so callers can localize the bad range.
func DecodeHeader(buf []byte) (AccessUnit, error) {
	if len(buf) < HeaderSize {
		return AccessUnit{}, fmt.Errorf("%w: got %d bytes", ErrTruncatedHeader, len(buf))
	}
	if string(buf[0:4]) != Magic {
		return AccessUnit{}, ErrBadMagic
	}
	if buf[4] != Version {
		return AccessUnit{}, fmt.Errorf("%w: %d", ErrUnsupportedVer, buf[4])
	}
	u := AccessUnit{
		IndexInGOP: binary.LittleEndian.Uint16(buf[6:8]),
		Index:      binary.LittleEndian.Uint32(buf[8:12]),
		GOPID:      binary.LittleEndian.Uint32(buf[12:16]),
		PayloadLen: binary.LittleEndian.Uint32(buf[16:20]),
		PTSMS:      int64(binary.LittleEndian.Uint64(buf[20:28])),
		KeyFrame:   buf[5]&KeyFrameFlag != 0,
		CRC:        binary.LittleEndian.Uint32(buf[32:36]),
	}
	want := crc32.ChecksumIEEE(buf[:32])
	u.CRCValid = want == u.CRC
	if !u.CRCValid {
		return u, fmt.Errorf("%w: stored=%08x calculated=%08x", ErrBadCRC, u.CRC, want)
	}
	return u, nil
}

// ScanHeaders resynchronizes on magic and never lets a truncated/corrupt
// header consume a later complete access unit.
func ScanHeaders(data []byte) ([]AccessUnit, []HeaderError) {
	var units []AccessUnit
	var bad []HeaderError
	off := 0
	for off < len(data) {
		rel := bytes.Index(data[off:], []byte(Magic))
		if rel < 0 {
			break
		}
		pos := off + rel
		if pos+HeaderSize > len(data) {
			bad = append(bad, HeaderError{Offset: int64(pos), Kind: fault.TruncatedHeader, Err: ErrTruncatedHeader})
			break
		}
		u, err := DecodeHeader(data[pos : pos+HeaderSize])
		u.Offset = int64(pos)
		if err != nil {
			nextRel := bytes.Index(data[pos+4:], []byte(Magic))
			next := -1
			if nextRel >= 0 {
				next = pos + 4 + nextRel
			}
			kind := fault.BadCRC
			if errors.Is(err, ErrBadCRC) && next >= 0 && next-pos < HeaderSize {
				kind = fault.TruncatedHeader
				err = fmt.Errorf("%w: resync at %d", ErrTruncatedHeader, next)
			}
			bad = append(bad, HeaderError{Offset: int64(pos), Kind: kind, Err: err})
			if next < 0 {
				break
			}
			off = next
			continue
		}
		units = append(units, u)
		frameEnd := pos + HeaderSize + int(u.PayloadLen)
		if frameEnd > len(data) {
			bad = append(bad, HeaderError{
				Offset: int64(pos + HeaderSize),
				Kind:   fault.TruncatedHeader,
				Err:    io.ErrUnexpectedEOF,
			})
			break
		}
		off = frameEnd
	}
	return units, bad
}

// LimitReader returns an error if more than max bytes are present. It avoids
// reading a >2 GiB input into memory.
func LimitReader(r io.Reader, max int64) io.Reader {
	return &io.LimitedReader{R: r, N: max + 1}
}

// CheckSize reports whether n exceeds max.
func CheckSize(n, max int64) error {
	if n > max {
		return fmt.Errorf("media: input size %d exceeds limit %d", n, max)
	}
	return nil
}
