// Package source reads frame records from a local file in bounded memory.
package source

import (
	"bufio"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"

	"mediq/internal/model"
)

// Record is one step of the scan: either a decoded frame or a diagnosable
// fault. The scan continues past recoverable faults; a non-recoverable fault
// ends the stream (the next Read returns io.EOF).
type Record struct {
	Frame *model.Frame
	Fault *model.Fault
}

// Reader streams frame records from a file. It holds at most one record in
// memory; callers own the returned payload buffer.
type Reader struct {
	f      *os.File
	br     *bufio.Reader
	offset int64 // byte offset of the next record header
	cfg    Options
	eof    bool
}

// Options bounds memory and configures diagnostics.
type Options struct {
	MaxFrameBytes    int
	MaxMetadataBytes int
}

func (o Options) withDefaults() Options {
	if o.MaxFrameBytes <= 0 {
		o.MaxFrameBytes = model.DefaultMaxFrameBytes
	}
	if o.MaxMetadataBytes <= 0 {
		o.MaxMetadataBytes = model.DefaultMaxMetadataBytes
	}
	return o
}

// Open opens path for sequential record reading.
func Open(path string, opts Options) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return NewReader(f, opts), nil
}

// NewReader wraps an already open file.
func NewReader(f *os.File, opts Options) *Reader {
	return &Reader{f: f, br: bufio.NewReaderSize(f, 256<<10), cfg: opts.withDefaults()}
}

func (r *Reader) Close() error { return r.f.Close() }

// Offset reports the byte offset of the next record to be read.
func (r *Reader) Offset() int64 { return r.offset }

// SeekStart positions the reader at a record boundary and clears EOF.
func (r *Reader) SeekStart(off int64) error {
	if _, err := r.f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	r.br.Reset(r.f)
	r.offset = off
	r.eof = false
	return nil
}

// PrefixCRC computes CRC-32 of bytes [0, n) without disturbing the reader's
// position. It proves a checkpoint belongs to this exact input prefix.
func (r *Reader) PrefixCRC(n int64) (uint32, error) {
	if n < 0 {
		return 0, fmt.Errorf("negative prefix length %d", n)
	}
	cur, err := r.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if _, err := r.f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	buf := make([]byte, 128<<10)
	var crc uint32
	for remaining := n; remaining > 0; {
		chunk := int64(len(buf))
		if remaining < chunk {
			chunk = remaining
		}
		if _, err := io.ReadFull(r.f, buf[:chunk]); err != nil {
			return 0, err
		}
		crc = crc32.Update(crc, crc32.IEEETable, buf[:chunk])
		remaining -= chunk
	}
	if _, err := r.f.Seek(cur, io.SeekStart); err != nil {
		return 0, err
	}
	return crc, nil
}

// Read returns the next record or io.EOF at the end of the stream. Faults are
// returned as records with a nil Frame, so no diagnostic is ever dropped for
// lack of channel space.
func (r *Reader) Read() (Record, error) {
	if r.eof {
		return Record{}, io.EOF
	}
	rec, err := r.readOne()
	if err != nil {
		return Record{}, err
	}
	if rec.Fault != nil && !rec.Fault.Resync() {
		r.eof = true
	}
	return rec, nil
}

// readOne decodes a single record or reports the first fault it hit.
func (r *Reader) readOne() (Record, error) {
	start := r.offset
	hdr := make([]byte, model.RecordHeaderSize)
	n, err := io.ReadFull(r.br, hdr)
	if err != nil {
		if errors.Is(err, io.EOF) && n == 0 {
			r.eof = true
			return Record{}, io.EOF
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			r.eof = true
			r.offset += int64(n)
			return Record{Fault: &model.Fault{
				Code:     model.CodeTruncatedHeader,
				Offset:   start,
				Expected: fmt.Sprint(model.RecordHeaderSize),
				Actual:   fmt.Sprint(n),
				Reason:   fmt.Sprintf("truncated record header; first missing byte at offset %d", start+int64(n)),
			}}, nil
		}
		return Record{}, err
	}

	rh, perr := model.ParseRecordHeader(hdr)
	if perr != nil {
		r.eof = true
		code := model.CodeBadHeader
		if string(hdr[0:4]) != model.RecordMagic {
			code = model.CodeBadMagic
		}
		return Record{Fault: &model.Fault{Code: code, Offset: start, Reason: perr.Error(), Err: perr}}, nil
	}
	r.offset += int64(model.RecordHeaderSize)

	if int64(rh.MetaLen) > int64(r.cfg.MaxMetadataBytes) {
		r.eof = true
		return Record{Fault: &model.Fault{
			Code: model.CodeOversizeMetadata, Seq: rh.Seq, HasSeq: true, Offset: start,
			Expected: fmt.Sprintf("<=%d", r.cfg.MaxMetadataBytes), Actual: fmt.Sprint(rh.MetaLen),
		}}, nil
	}
	if int64(rh.FrameLen) > int64(r.cfg.MaxFrameBytes) {
		r.eof = true
		return Record{Fault: &model.Fault{
			Code: model.CodeOversizeFrame, Seq: rh.Seq, HasSeq: true, Offset: start,
			Expected: fmt.Sprintf("<=%d", r.cfg.MaxFrameBytes), Actual: fmt.Sprint(rh.FrameLen),
		}}, nil
	}

	payloadLen := int64(rh.MetaLen) + int64(rh.FrameLen)
	payload := make([]byte, payloadLen)
	n, err = io.ReadFull(r.br, payload)
	r.offset += int64(n)
	if err != nil {
		r.eof = true
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			firstMissing := start + int64(model.RecordHeaderSize) + int64(n)
			return Record{Fault: &model.Fault{
				Code: model.CodeTruncatedPayload, Seq: rh.Seq, HasSeq: true, Offset: firstMissing,
				Expected: fmt.Sprintf("%d body bytes", payloadLen),
				Actual:   fmt.Sprint(n),
				Reason:   fmt.Sprintf("record body truncated; first missing byte at offset %d", firstMissing),
			}}, nil
		}
		return Record{}, err
	}

	return Record{Frame: &model.Frame{
		Seq:         rh.Seq,
		Timestamp:   rh.Timestamp,
		Timebase:    rh.Timebase,
		MetaLen:     rh.MetaLen,
		StartOffset: start,
		MetaOffset:  start + int64(model.RecordHeaderSize),
		FrameOffset: start + int64(model.RecordHeaderSize) + int64(rh.MetaLen),
		EndOffset:   start + int64(model.RecordHeaderSize) + payloadLen,
		Hash:        rh.Hash,
		Meta:        payload[:rh.MetaLen],
		Payload:     payload[rh.MetaLen:],
	}}, nil
}

// Digest accumulates a rolling CRC-32 over bytes consumed by a reader. It is
// a cheap identity check for checkpoint matching, not a content integrity
// check (frame hashes serve that purpose).
type Digest struct {
	crc  uint32
	size int64
}

func NewDigest() *Digest { return &Digest{} }

func (d *Digest) Write(p []byte) (int, error) {
	d.crc = crc32.Update(d.crc, crc32.IEEETable, p)
	d.size += int64(len(p))
	return len(p), nil
}

func (d *Digest) Sum() uint32 { return d.crc }
func (d *Digest) Size() int64 { return d.size }
