// Package wire parses the RLY1 relay record format, validates segment
// indexes and rebuilds them from record content when they are missing or
// damaged.
//
// File layout of a segment:
//
//	header  : "RLY1" magic, format version, track count, record count       (24 bytes)
//	index   : index page magic, entry count, then fixed 16-byte entries     (page based)
//	records : back to back records                                          (variable)
//
// Every record carries track id, sequence number, 32-bit PTS, payload length
// and CRC32. All parse errors are faults anchored at the original byte
// offset of the offending field or record start.
package wire

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"

	"relayaudit/internal/clock"
	"relayaudit/internal/fault"
)

const (
	// Magic is the segment header magic.
	Magic = "RLY1"
	// Version is the only supported format version.
	Version = 1
	// HeaderSize is the size of the segment header.
	HeaderSize = 24
	// MaxTracks is the largest sensible track count for a relay segment.
	MaxTracks = 16
	// MaxRecordPayload is the hard per-record payload ceiling (4 MiB).
	MaxRecordPayload = 4 << 20
	// RecordHeaderSize is track(1) seq(4) pts(4) length(4) = 13 bytes,
	// followed by payload and a 4-byte CRC32 trailer.
	RecordHeaderSize = 13
	// RecordTrailerSize is the CRC32 trailer length.
	RecordTrailerSize = 4
	// RecordOverhead is the fixed bytes around a payload.
	RecordOverhead = RecordHeaderSize + RecordTrailerSize
	// MaxRecordSize is the largest legal on-wire record.
	MaxRecordSize = MaxRecordPayload + RecordOverhead
	// MaxScanBytes caps how far index rebuild will scan past a damaged
	// region before giving up.
	MaxScanBytes = 64 << 20
	// MaxIndexPages bounds the index region accepted from disk.
	MaxIndexPages = 4096
)

// crcTable is the Castagnoli table used for record payload checksums.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Record is one decoded RLY1 record.
type Record struct {
	Track   uint8
	Seq     uint32
	PTS     clock.Stamp
	Payload []byte
	CRC     uint32

	// Offset is the original byte position of the record header.
	Offset int64
}

// Size returns the on-wire size of the record.
func (r *Record) Size() int64 {
	if r == nil {
		return 0
	}
	return int64(RecordOverhead + len(r.Payload))
}

// End returns the offset one past the record's last byte.
func (r *Record) End() int64 { return r.Offset + r.Size() }

// PayloadCRC computes the checksum over the stored payload bytes.
func PayloadCRC(payload []byte) uint32 {
	return crc32.Checksum(payload, crcTable)
}

// ValidCRC reports whether the stored checksum matches the payload.
func (r *Record) ValidCRC() bool { return PayloadCRC(r.Payload) == r.CRC }

// Marshal appends the encoded record to dst, reusing its capacity.
func (r *Record) Marshal(dst []byte) []byte {
	var hdr [RecordHeaderSize]byte
	hdr[0] = r.Track
	binary.BigEndian.PutUint32(hdr[1:5], r.Seq)
	binary.BigEndian.PutUint32(hdr[5:9], uint32(r.PTS))
	binary.BigEndian.PutUint32(hdr[9:13], uint32(len(r.Payload)))
	dst = append(dst, hdr[:]...)
	dst = append(dst, r.Payload...)
	var tail [RecordTrailerSize]byte
	binary.BigEndian.PutUint32(tail[:], PayloadCRC(r.Payload))
	return append(dst, tail[:]...)
}

// ErrShortHeader is returned when a header read hits EOF cleanly before any
// byte of the next record has been consumed.
var ErrShortHeader = io.ErrUnexpectedEOF

// RecordReader streams records from a byte slice.
type RecordReader struct {
	buf []byte
	off int64
	// limit bounds how far the reader may advance; it guards rebuild scans.
	limit int64
	// maxPayload rejects oversized records before allocation.
	maxPayload uint32
}

// NewRecordReader starts a reader over buf at start, bounded by end.
func NewRecordReader(buf []byte, start, end int64) *RecordReader {
	if end > int64(len(buf)) || end < start {
		end = int64(len(buf))
		if end < start {
			end = start
		}
	}
	return &RecordReader{buf: buf, off: start, limit: end, maxPayload: MaxRecordPayload}
}

// Offset returns the offset of the next record header.
func (r *RecordReader) Offset() int64 { return r.off }

// Remaining returns unread bytes before the limit.
func (r *RecordReader) Remaining() int64 { return r.limit - r.off }

// Next decodes one record. It returns io.EOF at a clean boundary and a
// fault describing the original offset otherwise.
func (r *RecordReader) Next() (*Record, error) {
	start := r.off
	if start >= r.limit {
		return nil, io.EOF
	}
	if r.limit-start < RecordHeaderSize {
		r.off = r.limit
		return nil, fault.New(fault.Truncated, start,
			"record header needs %d bytes, only %d remain", RecordHeaderSize, r.limit-start).
			WithLength(int64(RecordHeaderSize) - (r.limit - start))
	}
	hdr := r.buf[start : start+RecordHeaderSize]
	track := hdr[0]
	seq := binary.BigEndian.Uint32(hdr[1:5])
	pts := clock.Stamp(binary.BigEndian.Uint32(hdr[5:9]))
	n := binary.BigEndian.Uint32(hdr[9:13])

	if track == 0 || track > MaxTracks {
		r.off = r.limit
		return nil, fault.New(fault.Length, start, "illegal track id %d", track).WithTrack(track)
	}
	if n > r.maxPayload {
		r.off = r.limit
		return nil, fault.New(fault.Limit, start,
			"payload length %d exceeds %d byte ceiling", n, r.maxPayload).WithTrack(track)
	}
	total := int64(RecordOverhead) + int64(n)
	if total > r.limit-start {
		r.off = r.limit
		return nil, fault.New(fault.Truncated, start,
			"record of %d bytes cut at file end (%d remain)", total, r.limit-start).
			WithTrack(track).WithLength(total - (r.limit - start))
	}
	body := r.buf[start+RecordHeaderSize : start+RecordHeaderSize+int64(n)]
	want := binary.BigEndian.Uint32(r.buf[start+RecordHeaderSize+int64(n) : start+total])
	r.off = start + total

	if got := PayloadCRC(body); got != want {
		return nil, fault.New(fault.CRC, start,
			"payload crc %08x does not match stored %08x", got, want).
			WithTrack(track).WithLength(int64(n))
	}
	rec := &Record{Track: track, Seq: seq, PTS: pts, Payload: body, CRC: want, Offset: start}
	return rec, nil
}

// Resync scans forward from start for the next plausible record boundary.
// It is used after a damage event: the search is bounded by both the reader
// limit and MaxScanBytes, and accepts a candidate only when a full record
// with a valid CRC fits within the limit.
func (r *RecordReader) Resync(start int64) (int64, error) {
	scanned := int64(0)
	for cand := start + 1; cand+RecordHeaderSize+RecordTrailerSize <= r.limit; cand++ {
		if scanned++; scanned > MaxScanBytes {
			return 0, fault.New(fault.Limit, start, "resync scan exceeded %d byte budget", int64(MaxScanBytes))
		}
		track := r.buf[cand]
		if track == 0 || track > MaxTracks {
			continue
		}
		n := binary.BigEndian.Uint32(r.buf[cand+9 : cand+13])
		if n == 0 || n > r.maxPayload {
			continue
		}
		total := int64(RecordOverhead) + int64(n)
		if cand+total > r.limit {
			continue
		}
		body := r.buf[cand+RecordHeaderSize : cand+RecordHeaderSize+int64(n)]
		stored := binary.BigEndian.Uint32(r.buf[cand+RecordHeaderSize+int64(n) : cand+total])
		if PayloadCRC(body) == stored {
			return cand, nil
		}
	}
	return 0, fault.New(fault.Truncated, start, "no valid record boundary before end of input")
}

// IsFaultKind reports whether err is a fault of one of the given kinds.
func IsFaultKind(err error, kinds ...fault.Kind) bool {
	f, ok := fault.As(err)
	if !ok {
		return false
	}
	for _, k := range kinds {
		if f.Kind() == k {
			return true
		}
	}
	return false
}

var errNoRecords = errors.New("segment contains no records")
