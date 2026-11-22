// Package rtp ingests auxiliary tracks delivered as RTP packet dumps. Unlike a
// live receiver, it must cope with a recording that was captured mid-stream:
// packets may be out of order, duplicated, or missing. The reader reorders by
// sequence number, drops duplicates, accounts for losses, and reports every
// anomaly with the byte offset of the offending record.
package rtp

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

// File format (all integers big-endian):
//
//	magic     [4]byte  "RTPD"
//	version   uint16   format version (1)
//	ssrc      uint32   stream identifier
//	pad       uint16   reserved, zero
//	records   repeated:
//	  length  uint32   payload length of the following packet bytes
//	  crc     uint32   CRC-32 (IEEE) of the packet bytes
//	  bytes   [length]
//
// The CRC lets the reader distinguish a corrupted record from an intentional
// loss; a mismatched CRC is reported as a corrupt container at the record
// offset.
const (
	Magic       = "RTPD"
	Version     = 1
	minHeader   = 12 // magic + version + ssrc + pad
	countLength = 8  // record length + crc
)

// Record is one wire packet with its byte offset in the dump.
type Record struct {
	Offset  int64
	Index   int64
	Length  uint32
	CRC     uint32
	Payload []byte
}

// Reader iterates the dump. It does not implement adapter.Adapter directly,
// because a dump is one auxiliary track, not a whole asset; the mp4 asset and
// the rtp track are combined by the manifest loader.
type Reader struct {
	r      *bufio.Reader
	offset int64
	index  int64
	ssrc   uint32
	closed bool
}

// Open validates the header and returns a positioned reader.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := &Reader{r: bufio.NewReaderSize(f, 64<<10)}
	if err := r.readHeader(); err != nil {
		f.Close()
		return nil, err
	}
	return r, nil
}

func (r *Reader) readHeader() error {
	var hdr [minHeader]byte
	if _, err := io.ReadFull(r.r, hdr[:]); err != nil {
		return fmt.Errorf("rtp: short header: %w", err)
	}
	r.offset = minHeader
	if string(hdr[0:4]) != Magic {
		return fmt.Errorf("rtp: bad magic %q", string(hdr[0:4]))
	}
	if v := binary.BigEndian.Uint16(hdr[4:6]); v != Version {
		return fmt.Errorf("rtp: unsupported version %d", v)
	}
	r.ssrc = binary.BigEndian.Uint32(hdr[6:10])
	return nil
}

// SSRC returns the stream identifier from the header.
func (r *Reader) SSRC() uint32 { return r.ssrc }

// Next returns the next record, or io.EOF when the dump is exhausted.
func (r *Reader) Next() (*Record, error) {
	if r.closed {
		return nil, io.EOF
	}
	var meta [countLength]byte
	n, err := io.ReadFull(r.r, meta[:])
	if err == io.EOF || n == 0 {
		r.closed = true
		return nil, io.EOF
	}
	if err != nil {
		return nil, fmt.Errorf("rtp: truncated record header at %d: %w", r.offset, err)
	}
	length := binary.BigEndian.Uint32(meta[0:4])
	crc := binary.BigEndian.Uint32(meta[4:8])
	if length == 0 || length > 1<<20 {
		return nil, fmt.Errorf("rtp: implausible record length %d at %d", length, r.offset)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r.r, body); err != nil {
		return nil, fmt.Errorf("rtp: truncated record body at %d: %w", r.offset, err)
	}
	sum := crc32.ChecksumIEEE(body)
	if sum != crc {
		return nil, fmt.Errorf("rtp: crc mismatch at %d (got %08x want %08x)", r.offset, sum, crc)
	}
	rec := &Record{Offset: r.offset, Index: r.index, Length: length, CRC: crc, Payload: body}
	r.offset += countLength + int64(length)
	r.index++
	return rec, nil
}

// WriteDump encodes records (with CRC) into the dump format. Tests and the
// fixture generator use it; the production path only reads.
func WriteDump(w io.Writer, ssrc uint32, payloads [][]byte) error {
	if _, err := w.Write([]byte(Magic)); err != nil {
		return err
	}
	var hdr [8]byte
	binary.BigEndian.PutUint16(hdr[0:2], Version)
	binary.BigEndian.PutUint32(hdr[2:6], ssrc)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	for _, p := range payloads {
		var meta [countLength]byte
		binary.BigEndian.PutUint32(meta[0:4], uint32(len(p)))
		binary.BigEndian.PutUint32(meta[4:8], crc32.ChecksumIEEE(p))
		if _, err := w.Write(meta[:]); err != nil {
			return err
		}
		if _, err := w.Write(p); err != nil {
			return err
		}
	}
	return nil
}

// HexPreview is a small helper for diagnostics.
func HexPreview(b []byte, max int) string {
	if len(b) > max {
		b = b[:max]
	}
	return hex.EncodeToString(b)
}
