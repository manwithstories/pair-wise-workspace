// Package index validates keyframe index files against a scanned bitstream and
// repairs missing or out-of-range pages from the scan. Every repaired entry
// keeps its origin and confidence so a reviewer can audit the result; uncertain
// boundaries are marked for review instead of being silently accepted.
package index

import (
	"fmt"

	"archive.local/recover/internal/errs"
	"archive.local/recover/internal/stream"
)

// Limits keep hostile index files from allocating without bound.
const (
	MaxEntries   = 1_000_000
	MaxIndexSize = 64 << 20
)

// Magic is the on-disk signature of an index page file.
var Magic = [4]byte{'I', 'D', 'X', '1'}

// Entry is one keyframe page record as found in the index file.
type Entry struct {
	Sample   int    `json:"sample"`
	Offset   int64  `json:"offset"`
	Size     int64  `json:"size"`
	PTS      int64  `json:"pts"` // source clock ticks
	PageName string `json:"page,omitempty"`
}

// Parsed is the raw index after structural parsing.
type Parsed struct {
	Timescale uint32
	Entries   []Entry
	// Truncated reports a header or entry region cut short.
	Truncated bool
	// issues records structural parse findings so Validate can carry them over.
	issues errs.List
}

// Source describes where a repaired entry came from.
type Source string

const (
	SourceOriginal Source = "original"
	SourceScan     Source = "scan"
	SourcePending  Source = "pending_review"
)

// RepairedEntry is the final, auditable index record.
type RepairedEntry struct {
	Sample     int    `json:"sample"`
	Offset     int64  `json:"offset"`
	PTS        int64  `json:"pts"`
	Source     Source `json:"source"`
	Confidence string `json:"confidence"` // high, medium, low
	Note       string `json:"note,omitempty"`
}

// RepairResult is the validator's complete output.
type RepairResult struct {
	Entries  []RepairedEntry `json:"entries"`
	Original int             `json:"original_entries"`
	Repaired int             `json:"repaired_entries"`
	Pending  int             `json:"pending_review"`
	Dropped  int             `json:"dropped_entries"`
	Missing  int             `json:"missing_pages"`
	Issues   errs.List       `json:"issues"`
}

// Validate checks an index against the unit list scanned from the target
// bitstream, repairs gaps from the scan, and classifies each failure.
//
// The index header is a fixed 12-byte record:
//
//	magic[4] | version u16 | reserved u16 | timescale u32
//
// followed by 24-byte little-endian entries:
//
//	sample u32 | offset u64 | size u32 | pts u64
type ParseResult struct {
	Parsed
	Issues errs.List
}

// Parse reads the fixed binary index format with bounds checks at every step.
func Parse(data []byte, file string) (res *ParseResult) {
	res = &ParseResult{}
	defer func() { res.issues = append(errs.List(nil), res.Issues...) }()
	loc := func(off int64) errs.Location {
		return errs.Location{File: file, Offset: off, Sample: -1}
	}
	if len(data) > MaxIndexSize {
		res.Issues.AddFatal(errs.ClassCorrupt, errs.CodeIndexTooLarge,
			fmt.Sprintf("index size %d exceeds limit %d", len(data), MaxIndexSize), loc(0))
		return res
	}
	const hdr = 12
	if len(data) < hdr {
		res.Issues.AddFatal(errs.ClassTruncated, errs.CodeIndexHeaderTruncated,
			fmt.Sprintf("index header needs %d bytes, got %d", hdr, len(data)), loc(0))
		return res
	}
	if [4]byte{data[0], data[1], data[2], data[3]} != Magic {
		res.Issues.AddFatal(errs.ClassCorrupt, errs.CodeIndexHeaderBadMagic,
			"index magic mismatch", loc(0))
		return res
	}
	res.Timescale = uint32(data[8]) | uint32(data[9])<<8 | uint32(data[10])<<16 | uint32(data[11])<<24
	const entrySize = 24
	body := data[hdr:]
	count := len(body) / entrySize
	rem := len(body) % entrySize
	if count > MaxEntries {
		res.Issues.AddFatal(errs.ClassCorrupt, errs.CodeIndexTooLarge,
			fmt.Sprintf("%d entries exceeds limit %d", count, MaxEntries), loc(0))
		return res
	}
	if rem != 0 {
		res.Issues.Add(errs.ClassTruncated, errs.CodeIndexEntryOverrun,
			fmt.Sprintf("%d trailing bytes do not form a complete entry", rem),
			loc(int64(hdr+count*entrySize)))
		res.Truncated = true
	}
	for i := 0; i < count; i++ {
		b := body[i*entrySize:]
		e := Entry{
			Sample: int(le32(b[0:])),
			Offset: int64(le64(b[4:])),
			Size:   int64(le32(b[12:])),
			PTS:    int64(le64(b[16:])),
		}
		res.Entries = append(res.Entries, e)
	}
	return res
}

// Encode serializes an index in the same fixed format, used by fixtures.
func Encode(timescale uint32, entries []Entry) []byte {
	out := make([]byte, 0, 12+24*len(entries))
	out = append(out, Magic[:]...)
	out = append(out, 1, 0, 0, 0)
	out = append(out, byte(timescale), byte(timescale>>8), byte(timescale>>16), byte(timescale>>24))
	for _, e := range entries {
		var b [24]byte
		put32(b[0:], uint32(e.Sample))
		put64(b[4:], uint64(e.Offset))
		put32(b[12:], uint32(e.Size))
		put64(b[16:], uint64(e.PTS))
		out = append(out, b[:]...)
	}
	return out
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
func le64(b []byte) uint64 {
	return uint64(le32(b)) | uint64(le32(b[4:]))<<32
}
func put32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}
func put64(b []byte, v uint64) {
	put32(b, uint32(v))
	put32(b[4:], uint32(v>>32))
}

// Validate reconciles a parsed index with the scanned stream units.
//
// covered maps sample index to an input artifact name; it is used only to make
// findings more auditable and may be nil.
func Validate(p *Parsed, scan *stream.Result, file string) *RepairResult {
	res := &RepairResult{}
	if p != nil {
		res.Original = len(p.Entries)
	}
	if scan == nil {
		res.Issues.AddFatal(errs.ClassDependency, errs.CodeDependencyMissing,
			"index validation requires a scanned bitstream", errs.Location{File: file, Sample: -1, Offset: -1})
		return res
	}
	if p == nil {
		res.Issues.AddFatal(errs.ClassCorrupt, errs.CodeIndexHeaderTruncated,
			"no index parsed", errs.Location{File: file, Sample: -1, Offset: -1})
		return res
	}
	res.Issues = append(res.Issues, p.issues...)
	if p.Timescale == 0 {
		res.Issues.Add(errs.ClassCorrupt, errs.CodeIndexHeaderBadMagic,
			"index timescale is zero", errs.Location{File: file, Sample: -1, Offset: -1})
	}
	streamLen := int64(scan.Bytes)
	// Index of the last unit that ends at or before an offset; defines which
	// scanned keyframe a stale index offset belongs to.
	bySample := map[int]bool{}
	byOffset := map[int64]int{}
	for i, e := range p.Entries {
		loc := errs.Location{File: file, Sample: i, Offset: e.Offset}
		if e.Sample < 0 {
			res.Issues.AddFatal(errs.ClassOutOfRange, errs.CodeIndexOffsetOutOfRange,
				fmt.Sprintf("negative sample index %d", e.Sample), loc)
			res.Dropped++
			continue
		}
		if e.Offset < 0 || e.Offset >= streamLen {
			res.Issues.Add(errs.ClassOutOfRange, errs.CodeIndexOffsetOutOfRange,
				fmt.Sprintf("offset %d outside stream [0,%d)", e.Offset, streamLen), loc)
			res.Dropped++
			continue
		}
		if e.Size <= 0 || e.Size > streamLen-e.Offset {
			// A zero or overrunning size cannot be trusted, but the offset can
			// still anchor a scan-derived entry.
			res.Issues.Add(errs.ClassOutOfRange, errs.CodeIndexEntryOverrun,
				fmt.Sprintf("size %d does not fit at offset %d (stream %d bytes)", e.Size, e.Offset, streamLen), loc)
			res.Dropped++
			continue
		}
		if prev, ok := bySample[e.Sample]; ok && prev {
			res.Issues.Add(errs.ClassCorrupt, errs.CodeIndexDuplicateSample,
				fmt.Sprintf("duplicate sample index %d", e.Sample), loc)
			res.Dropped++
			continue
		}
		if prev, ok := byOffset[e.Offset]; ok {
			res.Issues.Add(errs.ClassCorrupt, errs.CodeIndexOffsetOverlap,
				fmt.Sprintf("offset %d already claimed by sample %d", e.Offset, prev), loc)
			res.Dropped++
			continue
		}
		bySample[e.Sample] = true
		byOffset[e.Offset] = e.Sample
	}

	// Entries that survive must also be ordered consistently: a higher sample
	// ordinal may never point earlier in the bitstream than a lower one. An
	// inversion is a stale or misassembled page, so the later entry is dropped
	// and recovered from the scan.
	lastOffset := int64(-1)
	for _, e := range p.Entries {
		if !bySample[e.Sample] {
			continue
		}
		if e.Offset < lastOffset {
			res.Issues.Add(errs.ClassCorrupt, errs.CodeIndexOffsetOutOfRange,
				fmt.Sprintf("sample %d offset %d precedes the prior entry's %d", e.Sample, e.Offset, lastOffset),
				errs.Location{File: file, Sample: e.Sample, Offset: e.Offset})
			delete(bySample, e.Sample)
			delete(byOffset, e.Offset)
			res.Dropped++
			continue
		}
		lastOffset = e.Offset
	}

	// Rebuild in sample order, filling gaps from scanned keyframes.
	keyframes := scan.KeyframeOffsets()
	maxSample := -1
	for _, e := range p.Entries {
		if e.Sample > maxSample {
			maxSample = e.Sample
		}
	}
	// If the index was truncated, or too many entries were rejected, fall back
	// to the full set of scanned keyframes as the expected page count.
	want := maxSample + 1
	if p.Truncated || want <= 0 || res.Dropped*2 >= len(p.Entries) {
		want = len(keyframes)
		if want == 0 {
			want = len(scan.Units)
		}
	}
	// A corrupt or hostile index can claim an arbitrarily large sample number.
	// The repaired index can never legitimately be longer than the number of
	// scanned keyframes (or units when the scan found no keyframes), so clamp
	// to that before allocating: this is what keeps a 12-byte index from
	// producing billions of rows.
	limit := len(keyframes)
	if limit == 0 {
		limit = len(scan.Units)
	}
	if limit == 0 {
		limit = 1
	}
	if want > limit {
		res.Issues.Add(errs.ClassOutOfRange, errs.CodeIndexOffsetOutOfRange,
			fmt.Sprintf("index claims %d samples but the bitstream has only %d switch points; clamped", want, limit),
			errs.Location{File: file, Sample: limit, Offset: -1})
		want = limit
	}
	accepted := map[int]RepairedEntry{}
	for _, e := range p.Entries {
		if !bySample[e.Sample] {
			continue
		}
		accepted[e.Sample] = RepairedEntry{
			Sample: e.Sample, Offset: e.Offset, PTS: e.PTS,
			Source: SourceOriginal, Confidence: "high",
		}
	}
	// Map scanned keyframes to positions by their order among keyframes.
	for i := 0; i < want; i++ {
		a, hadEntry := accepted[i]
		if hadEntry && isKeyframeOffset(keyframes, a.Offset) {
			res.Entries = append(res.Entries, a)
			continue
		}
		if hadEntry {
			// The offset is inside the stream but does not start a keyframe.
			// Drop the original and recover the boundary from the scan.
			res.Issues.Add(errs.ClassCorrupt, errs.CodeIndexOffsetOutOfRange,
				fmt.Sprintf("sample %d offset %d is not a scanned keyframe", i, a.Offset),
				errs.Location{File: file, Sample: i, Offset: a.Offset})
			res.Dropped++
		}
		// Missing page: recover the nearest scanned keyframe by ordinal.
		res.Missing++
		idx := i
		if idx >= len(keyframes) {
			res.Issues.Add(errs.ClassOutOfRange, errs.CodeIndexMissingPage,
				fmt.Sprintf("no scanned keyframe available for sample %d", i),
				errs.Location{File: file, Sample: i, Offset: -1})
			res.Pending++
			res.Entries = append(res.Entries, RepairedEntry{
				Sample: i, Offset: -1, Source: SourcePending, Confidence: "low",
				Note: "no candidate keyframe in scan",
			})
			continue
		}
		off := keyframes[idx]
		// A repair is certain when the scan supplies exactly the keyframe the
		// index expected. When the index was truncated or an entry was rejected
		// the ordinal mapping is only a candidate, so it stays pending review.
		src, conf := SourceScan, "high"
		if p.Truncated || res.Dropped > 0 {
			src, conf = SourcePending, "low"
			res.Issues.Add(errs.ClassTruncated, errs.CodeIndexPendingReview,
				fmt.Sprintf("repaired sample %d from scan offset %d needs review", i, off),
				errs.Location{File: file, Sample: i, Offset: off})
		} else {
			res.Issues.Add(errs.ClassCorrupt, errs.CodeIndexMissingPage,
				fmt.Sprintf("sample %d missing from index; recovered from scan", i),
				errs.Location{File: file, Sample: i, Offset: off})
		}
		res.Repaired++
		res.Entries = append(res.Entries, RepairedEntry{
			Sample: i, Offset: off, Source: src, Confidence: conf,
			Note: "offset recovered by scanning the bitstream",
		})
		if src == SourcePending {
			res.Pending++
		}
	}
	// PTS must never regress within an original-sourced run. A drop of more
	// than half the 32-bit range is a counter wrap, not corruption; it is
	// classified as a recoverable clock event so operators can tell them apart.
	const wrapThreshold = int64(1) << 31
	for i := 1; i < len(res.Entries); i++ {
		a, b := res.Entries[i-1], res.Entries[i]
		if a.Source != SourceOriginal || b.Source != SourceOriginal || b.PTS >= a.PTS {
			continue
		}
		loc := errs.Location{File: file, Sample: i, Offset: b.Offset}
		if a.PTS-b.PTS > wrapThreshold {
			res.Issues.Add(errs.ClassClock, errs.CodeClockWrap,
				fmt.Sprintf("index PTS wrapped between samples %d and %d", i-1, i), loc)
			continue
		}
		res.Issues.Add(errs.ClassClock, errs.CodeIndexPTSRegressed,
			fmt.Sprintf("PTS regressed between samples %d and %d", i-1, i), loc)
	}
	return res
}

// isKeyframeOffset reports whether off is one of the scanned keyframe offsets.
func isKeyframeOffset(kf []int64, off int64) bool {
	lo, hi := 0, len(kf)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		switch {
		case kf[mid] == off:
			return true
		case kf[mid] < off:
			lo = mid + 1
		default:
			hi = mid - 1
		}
	}
	return false
}
