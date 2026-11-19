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

// Reader streams frame records from a file. It never holds more than one
// record in memory; callers own the returned payload buffer.
type Reader struct {
	f       *os.File
	br      *bufio.Reader
	offset  int64 // byte offset of the next record header
	digest  *Digest
	cfg     Options
	faults  []*model.Fault
	faultCh chan<- *model.Fault
	done    bool
	eof     bool
}

type Options struct {
	MaxFrameBytes    int
	MaxMetadataBytes int
	// Faults, when non-nil, receives each recoverable fault as it is found.
	Faults chan<- *model.Fault
	// Digest, when non-nil, accumulates a rolling digest of the raw bytes.
	Digest *Digest
}

func openOptions(opts Options) Options {
	if opts.MaxFrameBytes <= 0 {
		opts.MaxFrameBytes = model.DefaultMaxFrameBytes
	}
	if opts.MaxMetadataBytes <= 0 {
		opts.MaxMetadataBytes = model.DefaultMaxMetadataBytes
	}
	return opts
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
	r := &Reader{f: f, br: bufio.NewReaderSize(f, 256<<10), cfg: openOptions(opts), digest: opts.Digest, faultCh: opts.Faults}
	return r
}

func (r *Reader) Close() error { return r.f.Close() }

// Offset reports the byte offset of the next record to be read.
func (r *Reader) Offset() int64 { return r.offset }

// Digest returns the rolling input digest, if one was supplied.
func (r *Reader) Digest() *Digest { return r.digest }

// Next returns the next valid record. Faults are reported (both via the
// channel and as the second return value) and scanning continues at the next
// recoverable boundary. io.EOF marks clean end of input.
func (r *Reader) Next() (*model.Frame, *model.Fault, error) {
	if r.eof {
		return nil, nil, io.EOF
	}
	for {
		fr, fault, err := r.readOne()
		if err != nil {
			return nil, fault, err
		}
		if fault != nil {
			r.report(fault)
			if !fault.Resync() {
				r.eof = true
				return nil, fault, nil // caller decides: frame lost, scanning stopped
			}
			continue
		}
		return fr, nil, nil
	}
}

func (r *Reader) report(f *model.Fault) {
	r.faults = append(r.faults, f)
	if r.faultCh != nil {
		select {
		case r.faultCh <- f:
		default:
		}
	}
}

// readOne reads one record. A returned fault with a nil error means the record
// was skipped; scanning may continue because the framing stayed in sync.
func (r *Reader) readOne() (*model.Frame, *model.Fault, error) {
	start := r.offset
	hdr := make([]byte, model.RecordHeaderSize)
	n, err := io.ReadFull(r.br, hdr)
	if err != nil {
		if errors.Is(err, io.EOF) && n == 0 {
			r.eof = true
			return nil, nil, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			r.eof = true
			if r.digest != nil {
				r.digest.Write(hdr[:n])
			}
			r.offset += int64(n)
			return nil, &model.Fault{
				Code:   model.CodeTruncatedHeader,
				Offset: start,
				Reason: fmt.Sprintf("record header needs %d bytes, got %d", model.RecordHeaderSize, n),
			}, nil
		}
		return nil, nil, err
	}
	if r.digest != nil {
		r.digest.Write(hdr)
	}
	rh, perr := model.ParseRecordHeader(hdr)
	if perr != nil {
		r.eof = true
		code := model.CodeBadHeader
		if string(hdr[0:4]) != model.RecordMagic {
			code = model.CodeBadMagic
		}
		return nil, &model.Fault{Code: code, Offset: start, Reason: perr.Error(), Err: perr}, nil
	}
	if int64(rh.MetaLen) > int64(r.cfg.MaxMetadataBytes) {
		r.eof = true
		return nil, &model.Fault{
			Code: model.CodeOversizeMetadata, Seq: rh.Seq, HasSeq: true, Offset: start,
			Expected: fmt.Sprintf("<=%d", r.cfg.MaxMetadataBytes),
			Actual:   fmt.Sprint(rh.MetaLen),
		}, nil
	}
	if int64(rh.FrameLen) > int64(r.cfg.MaxFrameBytes) {
		r.eof = true
		return nil, &model.Fault{
			Code: model.CodeOversizeFrame, Seq: rh.Seq, HasSeq: true, Offset: start,
			Expected: fmt.Sprintf("<=%d", r.cfg.MaxFrameBytes),
			Actual:   fmt.Sprint(rh.FrameLen),
		}, nil
	}

	payloadLen := int64(rh.MetaLen) + int64(rh.FrameLen)
	payload := make([]byte, payloadLen)
	r.offset += int64(model.RecordHeaderSize)
	n, err = io.ReadFull(r.br, payload)
	if r.digest != nil {
		r.digest.Write(payload[:n])
	}
	r.offset += int64(n)
	if err != nil {
		r.eof = true
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, &model.Fault{
				Code: model.CodeTruncatedPayload, Seq: rh.Seq, HasSeq: true, Offset: start + int64(model.RecordHeaderSize) + int64(n),
				Expected: fmt.Sprint(payloadLen),
				Actual:   fmt.Sprint(n),
				Reason:   fmt.Sprintf("record body truncated; first missing byte at offset %d", start+int64(model.RecordHeaderSize)+int64(n)),
			}, nil
		}
		return nil, nil, err
	}

	fr := &model.Frame{
		Seq:         rh.Seq,
		Timestamp:   rh.Timestamp,
		Timebase:    rh.Timebase,
		MetaLen:     rh.MetaLen,
		MetaOffset:  start + int64(model.RecordHeaderSize),
		FrameOffset: start + int64(model.RecordHeaderSize) + int64(rh.MetaLen),
		Hash:        rh.Hash,
		Payload:     payload[rh.MetaLen:],
	}
	return fr, nil, nil
}

// Digest accumulates a cheap rolling checksum over the exact bytes consumed.
// It is used by the checkpoint to prove it belongs to the same input file.
type Digest struct {
	crc  uint32
	size int64
}

func NewDigest() *Digest { return &Digest{} }

func (d *Digest) Write(p []byte) (int, error) {
	if d == nil {
		return len(p), nil
	}
	d.crc = crc32.Update(d.crc, crc32.IEEETable, p)
	d.size += int64(len(p))
	return len(p), nil
}

func (d *Digest) Sum() uint32 { return d.crc }
func (d *Digest) Size() int64 { return d.size }
func (d *Digest) Sum64() uint64 {
	return uint64(d.size)<<32 | uint64(d.crc)
}
