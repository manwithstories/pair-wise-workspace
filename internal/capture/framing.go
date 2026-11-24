package capture

import (
	"encoding/binary"
	"fmt"
	"io"

	"mediarig/internal/media"
)

// Record type bytes for the length-prefixed fixture container.
const (
	RecordRTP  byte = 1
	RecordRTCP byte = 2
)

const (
	// RecordHeaderSize is type byte + uint32 big-endian payload length.
	RecordHeaderSize = 5
	// MaxRecordSize is the hard per-record budget, including the 5-byte header.
	MaxRecordSize = 1 << 20
	// MaxRecordPayload leaves room for the header inside MaxRecordSize.
	MaxRecordPayload = MaxRecordSize - RecordHeaderSize
	// MaxInputBytes is the total accepted fixture/input stream size.
	MaxInputBytes int64 = 512 << 20
)

// Record is one framed raw protocol datagram.
type Record struct {
	Index   int
	Type    byte
	Payload []byte
}

// WriteRecord writes type, uint32 BE length, and payload.
func WriteRecord(w io.Writer, typ byte, payload []byte) error {
	if typ != RecordRTP && typ != RecordRTCP {
		return media.Corrupt(media.StageFraming, "unknown record type %d", typ)
	}
	if len(payload) > MaxRecordPayload {
		return media.Policy(media.StageInput, "record payload %d exceeds %d bytes", len(payload), MaxRecordPayload)
	}
	var hdr [RecordHeaderSize]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return media.InternalWrap(media.StageInput, err, "write record header")
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return media.InternalWrap(media.StageInput, err, "write record payload")
		}
	}
	return nil
}

// Reader reads bounded length-prefixed records.
type Reader struct {
	r         io.Reader
	recordIdx int
	total     int64
	maxInput  int64
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: r, maxInput: MaxInputBytes}
}

func (rr *Reader) SetMaxInput(n int64) *Reader {
	rr.maxInput = n
	return rr
}

// Next returns io.EOF only at a clean record boundary. A truncated header or
// payload is a graded corrupt-input error carrying the record index.
func (rr *Reader) Next() (Record, error) {
	var hdr [RecordHeaderSize]byte
	n, err := io.ReadFull(rr.r, hdr[:])
	if err == io.EOF && n == 0 {
		return Record{}, io.EOF
	}
	if err == io.ErrUnexpectedEOF || (err == nil && n < RecordHeaderSize) {
		return Record{}, media.WithRecordIndex(
			media.Corrupt(media.StageFraming, "truncated record header (%d/%d bytes)", n, RecordHeaderSize),
			rr.recordIdx,
		)
	}
	if err != nil {
		return Record{}, media.InternalWrap(media.StageInput, err, "read record header")
	}
	rr.total += int64(RecordHeaderSize)
	if rr.total > rr.maxInput {
		return Record{}, media.WithRecordIndex(
			media.Policy(media.StageInput, "input exceeds total limit %d bytes", rr.maxInput),
			rr.recordIdx,
		)
	}

	typ := hdr[0]
	if typ != RecordRTP && typ != RecordRTCP {
		return Record{}, media.WithRecordIndex(
			media.Corrupt(media.StageFraming, "illegal record type byte %d", typ),
			rr.recordIdx,
		)
	}
	size := int(binary.BigEndian.Uint32(hdr[1:]))
	if size > MaxRecordPayload {
		return Record{}, media.WithRecordIndex(
			media.Policy(media.StageInput, "record length %d exceeds payload limit %d", size, MaxRecordPayload),
			rr.recordIdx,
		)
	}
	rr.total += int64(size)
	if rr.total > rr.maxInput {
		return Record{}, media.WithRecordIndex(
			media.Policy(media.StageInput, "input exceeds total limit %d bytes", rr.maxInput),
			rr.recordIdx,
		)
	}

	payload := make([]byte, size)
	if size > 0 {
		n, err := io.ReadFull(rr.r, payload)
		if err != nil {
			seq := sequenceFromRaw(payload[:n])
			cerr := media.WithRecordIndex(
				media.CorruptWrap(media.StageFraming, err, "truncated record payload (%d/%d bytes)", n, size),
				rr.recordIdx,
			)
			if seq != nil {
				cerr = media.WithPacketSequence(cerr, *seq)
			}
			return Record{}, cerr
		}
	}
	rec := Record{Index: rr.recordIdx, Type: typ, Payload: payload}
	rr.recordIdx++
	return rec, nil
}

// ReadAll reads and minimally validates every record.
func ReadAll(r io.Reader) ([]Record, error) {
	cr := NewReader(r)
	var out []Record
	for {
		rec, err := cr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		if verr := ValidateRecordLength(rec); verr != nil {
			return out, verr
		}
		out = append(out, rec)
	}
}

// ValidateRecordLength rejects illegal empty/protocol packet lengths before
// invoking protocol parsers.
func ValidateRecordLength(rec Record) error {
	switch rec.Type {
	case RecordRTP:
		if len(rec.Payload) < 12 {
			err := media.Corrupt(media.StageRTP, "RTP packet length %d is shorter than 12-byte header", len(rec.Payload))
			err = media.WithRecordIndex(err, rec.Index)
			if seq := sequenceFromRaw(rec.Payload); seq != nil {
				err = media.WithPacketSequence(err, *seq)
			}
			return err
		}
		if rec.Payload[0]&0xc0 != 0x80 {
			err := media.Corrupt(media.StageRTP, "unsupported RTP version %d", rec.Payload[0]>>6)
			err = media.WithRecordIndex(err, rec.Index)
			if seq := sequenceFromRaw(rec.Payload); seq != nil {
				err = media.WithPacketSequence(err, *seq)
			}
			return err
		}
	case RecordRTCP:
		if len(rec.Payload) < 8 {
			return media.WithRecordIndex(
				media.Corrupt(media.StageRTCP, "RTCP packet length %d is shorter than 8 bytes", len(rec.Payload)),
				rec.Index,
			)
		}
	default:
		return media.WithRecordIndex(media.Corrupt(media.StageFraming, "unknown record type"), rec.Index)
	}
	return nil
}

func sequenceFromRaw(b []byte) *uint16 {
	if len(b) < 4 {
		return nil
	}
	seq := binary.BigEndian.Uint16(b[2:4])
	return &seq
}

// LimitDescription is used in stable terminal reports and tests.
func LimitDescription() string {
	return fmt.Sprintf("records<=%d bytes total<=%d bytes", MaxRecordSize, MaxInputBytes)
}
