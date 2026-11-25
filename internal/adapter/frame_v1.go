package adapter

import (
	"bytes"
	"crypto/sha256"
	"io"

	"avpipe/internal/media"
)

// frameV1Parser parses version-1 AVPI containers.
type frameV1Parser struct{}

func (p *frameV1Parser) Version() uint8 { return ContainerVersion1 }

type v1Header struct {
	flags       uint8
	recordCount uint32
	indexOffset uint64
	audioOffset uint64
}

func (p *frameV1Parser) Parse(r io.Reader, size int64) (*ParseResult, error) {
	rs := r.(io.ReadSeeker)
	h, err := readHeader(r, size)
	if err != nil {
		return nil, err
	}
	res := &ParseResult{}

	sectionEnd, err := sectionBoundary(size, h)
	if err != nil {
		return nil, err
	}

	off := int64(HeaderSize)
	var lastSeq uint32
	var lastPTS uint64
	haveLast := false
	for uint32(len(res.Frames)) < h.recordCount {
		if off >= sectionEnd {
			break
		}
		rec, nextOff, perr := parseRecord(rs, off, sectionEnd, size)
		if perr != nil {
			if perr == ErrShortStream {
				res.Errors = append(res.Errors, media.NewError("parser", media.KindTruncated,
					off, media.ActionTruncate, "tail cuts record %d", len(res.Frames)))
				break
			}
			me, ok := perr.(*media.Error)
			if !ok {
				return nil, perr
			}
			res.Errors = append(res.Errors, me)
			if me.Action == media.ActionFail {
				return nil, me
			}
			break
		}
		f := media.Frame{
			Sequence:      rec.seq,
			PTS90k:        rec.pts,
			PTSConfidence: media.ConfidenceVerified,
			Payload:       rec.payload,
		}
		if rec.checksumBad {
			f.Corrupt = true
			f.GapReason = media.KindChecksum
			f.Payload = nil
			res.Errors = append(res.Errors, media.NewSeqError("parser", media.KindChecksum,
				rec.seq, media.ActionGap, "sha256 mismatch, frame recorded as gap"))
		}
		if haveLast {
			if rec.seq <= lastSeq {
				f.Corrupt = true
				f.GapReason = "sequence_regression"
				res.Errors = append(res.Errors, media.NewSeqError("parser", "sequence_regression",
					rec.seq, media.ActionGap, "seq %d not greater than %d", rec.seq, lastSeq))
			}
			if !rec.checksumBad && rec.pts <= lastPTS {
				f.Corrupt = true
				f.GapReason = media.KindPTSRegression
				res.Errors = append(res.Errors, media.NewSeqError("parser", media.KindPTSRegression,
					rec.seq, media.ActionGap, "pts %d regresses from %d", rec.pts, lastPTS))
			}
		}
		if !f.Corrupt {
			lastSeq, lastPTS, haveLast = rec.seq, rec.pts, true
		}
		res.Frames = append(res.Frames, f)
		off = nextOff
	}

	if uint32(len(res.Frames)) < h.recordCount {
		res.Errors = append(res.Errors, media.NewError("parser", media.KindTruncated, off,
			media.ActionTruncate, "kept %d of %d declared records", len(res.Frames), h.recordCount))
	}

	parseIndex(rs, h, size, res)
	parseAudioV1(rs, h, size, res)
	parseFooter(rs, h, size, res)
	return res, nil
}

func readHeader(r io.Reader, size int64) (v1Header, error) {
	var hdr [HeaderSize]byte
	if err := readFull(r, 0, hdr[:], size); err != nil {
		return v1Header{}, media.NewError("parser", media.KindTruncated, 0, media.ActionFail,
			"header incomplete: %v", err)
	}
	h := v1Header{
		flags:       hdr[5],
		recordCount: u32(hdr[8:12]),
		indexOffset: u64(hdr[12:20]),
		audioOffset: u64(hdr[20:28]),
	}
	if h.recordCount == 0 {
		return h, media.NewError("parser", "empty_container", 0, media.ActionFail, "no records declared")
	}
	return h, nil
}

// sectionBoundary returns the end offset of the records section using
// overflow-safe arithmetic. The earliest of index/audio/end-of-file wins, so
// a truncated tail never lets the record loop run into another section.
func sectionBoundary(size int64, h v1Header) (int64, error) {
	end := uint64(size)
	if h.flags&FlagHasIndex != 0 && h.indexOffset > 0 && h.indexOffset < end {
		end = h.indexOffset
	}
	if h.flags&FlagHasAudio != 0 && h.audioOffset > 0 && h.audioOffset < end {
		end = h.audioOffset
	}
	if end < HeaderSize {
		return 0, media.NewError("parser", media.KindOutOfBounds, 0,
			media.ActionFail, "section boundary %d before header", end)
	}
	return int64(end), nil
}

type v1Record struct {
	seq         uint32
	pts         uint64
	payload     []byte
	checksumBad bool
}

func parseRecord(r io.ReadSeeker, off, sectionEnd, size int64) (v1Record, int64, error) {
	var rh [RecordHeaderSize]byte
	if err := readFull(r, off, rh[:], size); err != nil {
		return v1Record{}, off, wrapShort(err, off)
	}
	rec := v1Record{seq: u32(rh[0:4]), pts: u64(rh[4:12]), payload: nil}
	length := u32(rh[12:16])
	if length > MaxRecordSize {
		return rec, off, media.NewError("parser", media.KindLimitExceeded, off,
			media.ActionFail, "record length %d exceeds cap %d", length, MaxRecordSize)
	}
	total, overflow := media.AddU64(RecordHeaderSize, uint64(length))
	if overflow {
		return rec, off, media.NewError("parser", media.KindOverflow, off, media.ActionFail, "record size overflow")
	}
	total, overflow = media.AddU64(total, sha256.Size)
	if overflow {
		return rec, off, media.NewError("parser", media.KindOverflow, off, media.ActionFail, "record size overflow")
	}
	if !media.CheckBounds(uint64(off), total, uint64(sectionEnd)) {
		return rec, off, media.NewError("parser", media.KindTruncated, off,
			media.ActionTruncate, "record of %d bytes crosses section end", total)
	}
	buf := make([]byte, length+sha256.Size)
	if err := readFull(r, off+RecordHeaderSize, buf, size); err != nil {
		return rec, off, wrapShort(err, off)
	}
	rec.payload = buf[:length]
	want := buf[length:]
	got := sha256.Sum256(rec.payload)
	rec.checksumBad = !bytes.Equal(got[:], want)
	return rec, off + int64(total), nil
}

func wrapShort(err error, off int64) error {
	if me, ok := err.(*media.Error); ok {
		return me
	}
	return media.NewError("parser", "io_error", off, media.ActionFail, "%v", err)
}
