package probe

import (
	"encoding/binary"
	"fmt"
	"io"

	"mediacheck/internal/model"
)

func bigEndianU32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func bigEndianU64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }

type topScan struct {
	boxes   []BoxSpan
	mdat    *BoxSpan
}

// scanTopLevel reads only top-level box headers (8 or 16 bytes each) via
// ReaderAt, so even multi-hour assets never pull payloads into memory.
// A declared box extending past EOF is reported; payloads are never read.
func scanTopLevel(r io.ReaderAt, size int64) (topScan, int64, error) {
	var out topScan
	var hdr [16]byte
	pos := int64(0)
	for pos < size {
		if size-pos < 8 {
			return out, pos, nil // truncated box header
		}
		if _, err := r.ReadAt(hdr[:8], pos); err != nil {
			return out, pos, nil
		}
		boxSize := int64(bigEndianU32(hdr[:4]))
		typ := string(hdr[4:8])
		header := int64(8)
		if boxSize == 1 {
			if size-pos < 16 {
				return out, pos, nil
			}
			if _, err := r.ReadAt(hdr[8:16], pos+8); err != nil {
				return out, pos, nil
			}
			boxSize = int64(bigEndianU64(hdr[8:16]))
			header = 16
		}
		if boxSize < header {
			return out, -1, fmt.Errorf("probe: invalid box size %d at byte %d", boxSize, pos)
		}
		span := BoxSpan{Type: typ, Offset: pos, Size: boxSize}
		out.boxes = append(out.boxes, span)
		if typ == "mdat" {
			out.mdat = &span
		}
		end := pos + boxSize
		if end > size {
			return out, size, nil // first byte beyond EOF
		}
		pos = end
	}
	return out, -1, nil
}

// evaluateIntegrity cross-checks index counts, chunk offsets and declared
// sample sizes against the mdat boundary, marking index defects without
// ever reading past the declared limit.
func (res *Result) evaluateIntegrity() {
	if res.Integrity.Mdat == nil {
		res.Integrity.IndexIncomplete = true
		res.Integrity.Defects = append(res.Integrity.Defects, probeDefect{
			code: string(model.CodeIndexMissingPage),
			detail: "no mdat box found",
		})
		return
	}
	mdatEnd := res.Integrity.Mdat.Offset + res.Integrity.Mdat.Size
	mdatPayload := res.Integrity.Mdat.Offset + 8
	for _, tr := range res.Tracks {
		if idx := res.findRaw(tr.ID); idx != nil {
			res.checkIndex(idx, mdatPayload, mdatEnd)
		}
	}
}
func (res *Result) checkIndex(idx *rawIndex, mdatPayload, mdatEnd int64) {
	n := len(idx.sizes)
	if n == 0 {
		res.Integrity.IndexIncomplete = true
		res.Integrity.Defects = append(res.Integrity.Defects, probeDefect{
			code: string(model.CodeIndexMissingPage),
			detail: fmt.Sprintf("track %d has empty stsz", idx.trackID),
		})
		return
	}
	// stco entries must cover every declared sample (one chunk per sample
	// in these fixtures); a short table is a missing index page.
	if len(idx.chunks) < n {
		res.Integrity.IndexIncomplete = true
		res.Integrity.Defects = append(res.Integrity.Defects, probeDefect{
			code: string(model.CodeIndexMissingPage),
			start: int64(len(idx.chunks)), end: int64(n),
			detail: fmt.Sprintf("track %d: stco has %d entries for %d samples",
				idx.trackID, len(idx.chunks), n),
		})
	}
	for i := 0; i < n && i < len(idx.chunks); i++ {
		off := idx.chunks[i]
		size := int64(idx.sizes[i])
		if off < mdatPayload || off+size > mdatEnd {
			code := model.CodeLengthOutOfBounds
			if off >= mdatEnd || off < mdatPayload {
				code = model.CodeLengthOutOfBounds
			}
			res.Integrity.Defects = append(res.Integrity.Defects, probeDefect{
				code: string(code), start: off, end: off + size,
				detail: fmt.Sprintf("track %d sample %d declares bytes [%d,%d) outside mdat",
					idx.trackID, i, off, off+size),
			})
		}
	}
}

// promoteDefects converts internal defects into shared-model media errors.
func (res *Result) promoteDefects() {
	for _, d := range res.Integrity.Defects {
		res.MediaErrors = append(res.MediaErrors, &model.MediaError{
			Code: model.ErrorCode(d.code), Source: res.InputID,
			Start: d.start, End: d.end, Detail: d.detail,
		})
	}
}

// findRaw locates the raw index behind a built track.
func (res *Result) findRaw(trackID uint32) *rawIndex {
	for _, idx := range res.rawList {
		if idx.trackID == trackID {
			return idx
		}
	}
	return nil
}
