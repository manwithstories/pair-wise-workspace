package mp4

import (
	"io"

	gomp4 "github.com/abema/go-mp4"

	"medialine/internal/fault"
)

// childBox reads and validates the box header at off, which must lie inside
// the range [parentStart, parentEnd). It never reads payload bytes.
func childBox(r io.ReadSeeker, off, parentStart, parentEnd, maxSize uint64) (*gomp4.BoxInfo, error) {
	if off < parentStart || off+8 > parentEnd {
		return nil, fault.New(fault.StageParse, fault.KindTruncatedBox,
			"box header at %d lies outside [%d,%d)", off, parentStart, parentEnd).
			At(fault.UnknownTrack, int64(off))
	}
	if _, err := r.Seek(int64(off), io.SeekStart); err != nil {
		return nil, fault.From(err, fault.StageParse, fault.KindTruncatedBox).At(fault.UnknownTrack, int64(off))
	}
	bi, err := gomp4.ReadBoxInfo(r)
	if err != nil {
		return nil, fault.From(err, fault.StageParse, fault.KindTruncatedBox).At(fault.UnknownTrack, int64(off))
	}
	if bi.Size < bi.HeaderSize || bi.HeaderSize > 16 {
		return nil, fault.New(fault.StageParse, fault.KindInvalidLength,
			"box %s at %d declares size %d", bi.Type, off, bi.Size).At(fault.UnknownTrack, int64(off))
	}
	if bi.Size > maxSize {
		return nil, fault.New(fault.StageParse, fault.KindInvalidLength,
			"box %s size %d exceeds limit %d", bi.Type, bi.Size, maxSize).At(fault.UnknownTrack, int64(off))
	}
	if bi.Offset+bi.Size > parentEnd {
		kind := fault.KindInvalidLength
		if bi.Type == gomp4.BoxTypeMfra() {
			kind = fault.KindMissingIndex
		}
		return nil, fault.New(fault.StageParse, kind,
			"box %s at %d claims %d bytes but parent ends at %d", bi.Type, off, bi.Size, parentEnd).
			At(fault.UnknownTrack, int64(off))
	}
	return bi, nil
}

// childSpans enumerates the direct children of a container without expanding
// anything. A malformed child stops the walk and is returned to the caller.
func childSpans(r io.ReadSeeker, parentStart, parentEnd, maxSize uint64) ([]*gomp4.BoxInfo, error) {
	var out []*gomp4.BoxInfo
	for off := parentStart; off+8 <= parentEnd; {
		bi, err := childBox(r, off, parentStart, parentEnd, maxSize)
		if err != nil {
			return out, err
		}
		out = append(out, bi)
		off = bi.Offset + bi.Size
	}
	return out, nil
}

// readBoxPayload unmarshals a small leaf box.
func readBoxPayload(r io.ReadSeeker, bi *gomp4.BoxInfo, dst gomp4.IBox) error {
	if _, err := bi.SeekToPayload(r); err != nil {
		return fault.From(err, fault.StageParse, fault.KindTruncatedBox).At(fault.UnknownTrack, int64(bi.Offset))
	}
	if _, err := gomp4.Unmarshal(r, bi.Size-bi.HeaderSize, dst, bi.Context); err != nil {
		return fault.From(err, fault.StageParse, fault.KindCorruptSample).At(fault.UnknownTrack, int64(bi.Offset))
	}
	return nil
}

// resyncBox searches forward from start for the next plausible box header.
// Only the boxes medialine consumes are accepted, and each candidate must
// declare a size that fits inside the file and open with a valid child box.
// Resynchronising is what allows a file with one corrupt length declaration to
// still deliver its undamaged tracks.
func resyncBox(r io.ReadSeeker, start, fileSize uint64) (uint64, bool) {
	buf := make([]byte, 8)
	child := make([]byte, 8)
	for off := start; off+8 <= fileSize; off++ {
		if _, err := r.Seek(int64(off), io.SeekStart); err != nil {
			return 0, false
		}
		if _, err := io.ReadFull(r, buf); err != nil {
			return 0, false
		}
		typ := string(buf[4:8])
		if typ != "moov" && typ != "mfra" && typ != "ftyp" {
			continue
		}
		size := uint64(buf[0])<<24 | uint64(buf[1])<<16 | uint64(buf[2])<<8 | uint64(buf[3])
		if size < 16 || off+size > fileSize {
			continue
		}
		// Validate the first child so random payload bytes cannot pass.
		if off+16 > fileSize {
			continue
		}
		if _, err := r.Seek(int64(off+8), io.SeekStart); err != nil {
			return 0, false
		}
		if _, err := io.ReadFull(r, child); err != nil {
			return 0, false
		}
		ct := string(child[4:8])
		ok := false
		switch typ {
		case "moov":
			ok = ct == "mvhd" || ct == "trak" || ct == "mvex" || ct == "udta"
		case "mfra":
			ok = ct == "tfra" || ct == "mfro"
		case "ftyp":
			ok = ct == "isom" || ct == "iso6" || ct == "mp41" || ct == "mp42"
		}
		if ok {
			return off, true
		}
	}
	return 0, false
}
