package wire

import (
	"relayaudit/internal/fault"
)

// ScanResult is the outcome of a full segment scan: the parsed header, either
// the trusted on-disk index or a rebuilt one, the valid records in file order
// and every damage fault in original coordinates.
type ScanResult struct {
	Header       Header
	Index        *Index
	IndexRebuilt bool
	// IndexFault is the index problem that forced a rebuild, if any.
	IndexFault *fault.Fault
	// StaleIndex is set when the on-disk index parsed but disagreed with
	// record content; it is never trusted in that case.
	StaleIndex bool
	Records    []*Record
	Damage     []*fault.Fault
	SourceSize int64
}

// Scan validates the segment and produces the record inventory. It prefers the
// on-disk index but rebuilds deterministically whenever the index is missing,
// truncated, out of range or inconsistent with record content. Rebuilding
// never reads past the end of buf.
func Scan(buf []byte) (*ScanResult, error) {
	h, err := ParseHeader(buf)
	if err != nil {
		return nil, err
	}
	if err := h.ValidateBounds(int64(len(buf))); err != nil {
		return nil, err
	}
	res := &ScanResult{Header: h, SourceSize: int64(len(buf))}

	idx, ierr := ParseIndex(buf, h)
	if ierr == nil {
		if verr := VerifyIndex(idx, buf, h); verr != nil {
			res.StaleIndex = true
			res.IndexFault, _ = fault.As(verr)
			idx = nil
		}
	} else {
		res.IndexFault, _ = fault.As(ierr)
	}
	if idx == nil {
		rebuilt, recs, damage, rerr := Rebuild(buf, h, int64(len(buf)))
		if rerr != nil && recs == nil {
			return res, rerr
		}
		res.Index = rebuilt
		res.IndexRebuilt = true
		res.Damage = damage
		res.Records = make([]*Record, 0, len(recs))
		for _, r := range recs {
			res.Records = append(res.Records, r.Rec)
		}
		return res, nil
	}
	res.Index = idx
	res.Records = make([]*Record, 0, len(idx.Entries))
	for _, e := range idx.Entries {
		rec, rerr := readRecordAt(buf, e.Offset, int64(len(buf)))
		if rerr != nil {
			// The index was verified, so this cannot normally happen; treat
			// it as damage and rebuild rather than trust the cache.
			f, _ := fault.As(rerr)
			if f != nil {
				res.Damage = append(res.Damage, f)
			}
			continue
		}
		res.Records = append(res.Records, rec)
	}
	return res, nil
}

// VerifyRecordMatches checks that a scanned record still decodes from buf at
// its offset with the same fields. Used by verification to re-read outputs
// independently.
func VerifyRecordMatches(buf []byte, rec *Record) error {
	if rec.Offset < 0 || rec.End() > int64(len(buf)) {
		return fault.New(fault.Truncated, rec.Offset, "record [%d,%d) outside %d byte buffer",
			rec.Offset, rec.End(), len(buf))
	}
	got, err := readRecordAt(buf, rec.Offset, int64(len(buf)))
	if err != nil {
		return err
	}
	if got.Track != rec.Track || got.Seq != rec.Seq || got.PTS != rec.PTS || got.Size() != rec.Size() {
		return fault.New(fault.Index, rec.Offset, "record fields do not match expected inventory")
	}
	return nil
}
