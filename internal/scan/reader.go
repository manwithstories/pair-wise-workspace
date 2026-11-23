// Package scan reads medpipe containers record by record, verifies every
// checksum and distinguishes recoverable tail truncation from mid-stream
// corruption.
package scan

import (
	"encoding/binary"
	"io"

	"medpipe/internal/model"
)

// Result is the full scan outcome. A scan may carry both a mid-stream
// corruption and a tail truncation: callers inspect FirstCorruption to decide
// whether the input is still usable.
type Result struct {
	Frames             []*model.Frame
	Audio              []*model.AudioBlock
	FrameOffsets       []int64
	FirstCorruption    *model.LocatableError // first bad record, mid-stream
	TailTruncated      bool                  // last record ran past EOF
	RecoverableOffset  int64                 // offset of last complete record end
	RecoveredFrameNum  int                   // complete records kept at the tail
	TotalRecords       int
	CompleteRecords    int
}

// Header mirrors the 16-byte container header.
type Header struct {
	Version      byte
	RecordCount  uint32
	Flags        uint32
	Offset       int64
}

// Reader consumes a seekable source once. Use NewReader followed by Scan.
type Reader struct {
	r      io.ReadSeeker
	size   int64
	offset int64
}

// NewReader resolves the source size; the caller keeps ownership of r.
func NewReader(r io.ReadSeeker) (*Reader, error) {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return &Reader{r: r, size: size}, nil
}

// ReadHeader validates magic/version and returns the parsed header.
func (rd *Reader) ReadHeader() (*Header, error) {
	buf := make([]byte, model.HeaderSize)
	n, err := io.ReadFull(rd.r, buf)
	rd.offset += int64(n)
	if err != nil {
		return nil, model.NewError(model.ErrTruncated, "header short read: %v", err).At(rd.offset)
	}
	if string(buf[0:4]) != model.Magic {
		return nil, model.NewError(model.ErrHeader, "bad magic %q", buf[0:4]).At(0)
	}
	if buf[4] != model.Version {
		return nil, model.NewError(model.ErrHeader, "unsupported version %d", buf[4]).At(4)
	}
	return &Header{
		Version:     buf[4],
		RecordCount: binary.BigEndian.Uint32(buf[8:12]),
		Flags:       binary.BigEndian.Uint32(buf[12:16]),
		Offset:      rd.offset,
	}, nil
}

func (rd *Reader) readFull(buf []byte) (int, error) {
	n, err := io.ReadFull(rd.r, buf)
	rd.offset += int64(n)
	return n, err
}

// readRecord pulls one framed record. A short read at any stage means the
// final record is truncated; the reader position is left at EOF.
func (rd *Reader) readRecord() (kind, flags uint16, seq uint32, ts int64, meta, payload []byte, sum [32]byte, err error) {
	var head [4]byte
	if _, err = rd.readFull(head[:]); err != nil {
		return 0, 0, 0, 0, nil, nil, sum, model.NewError(model.ErrTruncated, "record size unreadable: %v", err).At(rd.offset)
	}
	recStart := rd.offset - 4
	recSize := int64(binary.BigEndian.Uint32(head[:]))
	// recSize includes the 4-byte size prefix; the body follows it.
	bodyLen := recSize - 4
	if bodyLen < 20+32 || recSize > 1<<24 {
		return 0, 0, 0, 0, nil, nil, sum, model.NewError(model.ErrKind, "implausible record size %d", recSize).At(recStart)
	}
	body := make([]byte, bodyLen)
	if _, err = rd.readFull(body); err != nil {
		return 0, 0, 0, 0, nil, nil, sum, model.NewError(model.ErrTruncated, "record body short: %v", err).At(rd.offset)
	}
	kind = binary.BigEndian.Uint16(body[0:2])
	flags = binary.BigEndian.Uint16(body[2:4])
	seq = binary.BigEndian.Uint32(body[4:8])
	ts = int64(binary.BigEndian.Uint64(body[8:16]))
	metaLen := int(binary.BigEndian.Uint32(body[16:20]))
	if 20+metaLen+4 > len(body)-32 {
		return 0, 0, 0, 0, nil, nil, sum, model.NewError(model.ErrKind, "meta length %d overflows record", metaLen).At(recStart)
	}
	meta = body[20 : 20+metaLen]
	payOff := 20 + metaLen
	payLen := int(binary.BigEndian.Uint32(body[payOff : payOff+4]))
	payStart := payOff + 4
	if payStart+payLen+32 != len(body) {
		return 0, 0, 0, 0, nil, nil, sum, model.NewError(model.ErrKind, "payload length inconsistent with frame").At(recStart)
	}
	payload = body[payStart : payStart+payLen]
	copy(sum[:], body[len(body)-32:])
	return kind, flags, seq, ts, append([]byte(nil), meta...), append([]byte(nil), payload...), sum, nil
}

// Scan consumes every record and verifies checksums. Tail truncation is a
// recoverable condition; a fully read record with a bad checksum or unknown
// kind is a mid-stream corruption and never silently skipped.
func (rd *Reader) Scan() (*Result, error) {
	if _, err := rd.ReadHeader(); err != nil {
		return nil, err
	}
	res := &Result{RecoverableOffset: rd.offset}
	for rd.offset < rd.size {
		recStart := rd.offset
		kind, flags, seq, ts, meta, payload, sum, rerr := rd.readRecord()
		if rerr != nil {
			if le, ok := model.AsLocatable(rerr); ok && le.Code == model.ErrTruncated {
				res.TailTruncated = true
				return res, nil
			}
			return res, rerr
		}
		res.TotalRecords++
		recEnd := rd.offset
		if kind != model.KindVideo && kind != model.KindAudio {
			res.recordCorruption(model.NewError(model.ErrKind, "unknown record kind %d", kind).At(recStart))
			res.markComplete(recEnd)
			continue
		}
		if cerr := model.VerifyChecksum(kind, flags, seq, ts, meta, payload, sum); cerr != nil {
			le, _ := model.AsLocatable(cerr)
			le.Offset = recStart
			res.recordCorruption(le)
			res.markComplete(recEnd)
			continue
		}
		if kind == model.KindVideo {
			f, derr := model.DecodeFrame(seq, ts, flags, meta, payload, sum, recStart)
			if derr != nil {
				res.recordCorruption(derr.(*model.LocatableError).At(recStart))
			} else {
				res.Frames = append(res.Frames, f)
				res.FrameOffsets = append(res.FrameOffsets, recStart)
			}
		} else {
			a, derr := model.DecodeAudio(seq, ts, meta, payload, sum, recStart)
			if derr != nil {
				res.recordCorruption(derr.(*model.LocatableError).At(recStart))
			} else {
				res.Audio = append(res.Audio, a)
			}
		}
		res.markComplete(recEnd)
	}
	return res, nil
}

func (res *Result) recordCorruption(e *model.LocatableError) {
	if res.FirstCorruption == nil {
		res.FirstCorruption = e
	}
}

func (res *Result) markComplete(recStart int64) {
	res.CompleteRecords++
	res.RecoverableOffset = recStart
	res.RecoveredFrameNum = res.CompleteRecords
}
