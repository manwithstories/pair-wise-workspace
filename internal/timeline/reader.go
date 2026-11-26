// Package timeline streams and validates cross-node NDJSON timeline records.
package timeline

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/relayteam/relay/internal/media"
	"github.com/relayteam/relay/internal/verify"
)

// Transition describes a clip transition endpoint. Endpoint must be present
// for every non-nil transition.
type Transition struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Endpoint string `json:"endpoint"`
	FromClip string `json:"from_clip,omitempty"`
	ToClip   string `json:"to_clip,omitempty"`
}

// Record is one NDJSON timeline observation.
type Record struct {
	Track      string      `json:"track"`
	Rendition  string      `json:"rendition"`
	Seq        int64       `json:"seq"`
	PTS90k     int64       `json:"pts90k"`
	Dur90k     int64       `json:"dur90k"`
	Clip       string      `json:"clip"`
	Transition *Transition `json:"transition,omitempty"`
	Payload    string      `json:"payload"`
	CRC32      uint32      `json:"crc32"`

	// Source location populated by Reader and excluded from NDJSON/CRC.
	RecordNo int `json:"-"`
	LineNo   int `json:"-"`
}

// Meta returns the shared error location for this record.
func (r Record) Meta(recordNo, lineNo int) media.Meta {
	return media.Meta{
		Track: r.Track, Rendition: r.Rendition,
		RecordNo: recordNo, LineNo: lineNo,
		Seq: r.Seq, PTS90k: media.Tick(r.PTS90k),
	}
}

// Canonical is the stable byte string protected by CRC32. The crc32 field is
// intentionally absent. Field order is part of the on-disk contract.
func (r Record) Canonical() []byte {
	var b strings.Builder
	_, _ = b.WriteString(r.Track)
	_ = b.WriteByte(0)
	_, _ = b.WriteString(r.Rendition)
	_ = b.WriteByte(0)
	_, _ = b.WriteString(strconv.FormatInt(r.Seq, 10))
	_ = b.WriteByte(0)
	_, _ = b.WriteString(strconv.FormatInt(r.PTS90k, 10))
	_ = b.WriteByte(0)
	_, _ = b.WriteString(strconv.FormatInt(r.Dur90k, 10))
	_ = b.WriteByte(0)
	_, _ = b.WriteString(r.Clip)
	_ = b.WriteByte(0)
	if r.Transition == nil {
		_, _ = b.WriteString("-")
	} else {
		t := r.Transition
		_, _ = b.WriteString(t.ID)
		_ = b.WriteByte(0)
		_, _ = b.WriteString(t.Kind)
		_ = b.WriteByte(0)
		_, _ = b.WriteString(t.Endpoint)
		_ = b.WriteByte(0)
		_, _ = b.WriteString(t.FromClip)
		_ = b.WriteByte(0)
		_, _ = b.WriteString(t.ToClip)
	}
	_ = b.WriteByte(0)
	_, _ = b.WriteString(r.Payload)
	return []byte(b.String())
}

// ComputeCRC fills and returns the record's IEEE CRC32.
func (r *Record) ComputeCRC() uint32 {
	r.CRC32 = verify.CRC32IEEE(r.Canonical())
	return r.CRC32
}

// Reader is a streaming NDJSON parser. It never retains the whole input.
type Reader struct {
	scanner   *bufio.Scanner
	lineNo    int
	recordNo  int
	verifyCRC bool
}

// ReaderOption configures the streaming reader.
type ReaderOption func(*Reader)

// WithoutCRC disables CRC verification (useful for low-level parser tests).
func WithoutCRC() ReaderOption {
	return func(r *Reader) { r.verifyCRC = false }
}

// NewReader wraps r with a one-megabyte maximum NDJSON line.
func NewReader(r io.Reader, opts ...ReaderOption) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	tr := &Reader{scanner: sc, verifyCRC: true}
	for _, opt := range opts {
		opt(tr)
	}
	return tr
}

// Counts returns the current record and physical line counters.
func (r *Reader) Counts() (recordNo, lineNo int) {
	return r.recordNo, r.lineNo
}

// Next returns the next record or io.EOF. Malformed input is a *media.Error
// carrying the first bad record/line position.
func (r *Reader) Next() (*Record, error) {
	for r.scanner.Scan() {
		line := r.scanner.Bytes()
		r.lineNo++
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		r.recordNo++
		meta := media.Meta{RecordNo: r.recordNo, LineNo: r.lineNo}
		var rec Record
		if err := json.Unmarshal([]byte(trimmed), &rec); err != nil {
			kind := media.ErrMalformedRecord
			if isTruncatedJSON(err, trimmed) {
				kind = media.ErrTruncatedRecord
			}
			return nil, media.E(kind, "timeline.reader", meta, err)
		}
		meta = rec.Meta(r.recordNo, r.lineNo)
		rec.RecordNo = r.recordNo
		rec.LineNo = r.lineNo
		if r.verifyCRC {
			want := verify.CRC32IEEE(rec.Canonical())
			if want != rec.CRC32 {
				err := media.E(media.ErrCRC32, "timeline.reader", meta, nil)
				err.Details = "crc32 field=" + strconv.FormatUint(uint64(rec.CRC32), 10) +
					" computed=" + strconv.FormatUint(uint64(want), 10)
				return &rec, err
			}
		}
		return &rec, nil
	}
	if err := r.scanner.Err(); err != nil {
		meta := media.Meta{RecordNo: r.recordNo + 1, LineNo: r.lineNo + 1}
		return nil, media.E(media.ErrMalformedRecord, "timeline.reader", meta, err)
	}
	return nil, io.EOF
}

func isTruncatedJSON(err error, line string) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "unexpected end of JSON input") ||
		strings.Contains(msg, "unexpected EOF") ||
		(!strings.HasSuffix(line, "}") && strings.Contains(msg, "EOF"))
}

// ReadAll drains a reader. Tests and small fixtures use it; production paths
// stream records through stream.Pipeline instead.
func ReadAll(r io.Reader, opts ...ReaderOption) ([]Record, error) {
	tr := NewReader(r, opts...)
	var out []Record
	for {
		rec, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			if rec != nil {
				out = append(out, *rec)
			}
			return out, err
		}
		if rec != nil {
			out = append(out, *rec)
		}
	}
}
