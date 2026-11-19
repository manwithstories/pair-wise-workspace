// Package mp4 scans local ISOBMFF files: track time bases, sample tables,
// keyframes and source byte offsets. The walker is deliberately independent of
// a decoder and tolerates truncation: a damaged file yields located faults
// rather than an error, so the recovery stage can still reason about it.
package mp4

import (
	"fmt"
	"io"
	"os"

	mp4 "github.com/abema/go-mp4"

	"archivearchivist/internal/model"
)

// box is one scanned box with its absolute byte extent.
type box struct {
	typ    mp4.BoxType
	path   []mp4.BoxType
	offset uint64
	size   uint64 // including header; clamped to the file on truncation
	header uint64
	trunc  bool
}

// Sample is one reconstructed sample-table entry.
type Sample struct {
	Index    uint32 `json:"index"`
	Offset   int64  `json:"offset"`
	Size     uint32 `json:"size"`
	DTS      int64  `json:"dts"`
	Duration int64  `json:"duration"`
	Keyframe bool   `json:"keyframe"`
	Corrupt  bool   `json:"corrupt"`
	Isolated bool   `json:"isolated"` // no usable duration, excluded from timing
}

// Track is one scanned media track.
type Track struct {
	ID             uint32   `json:"id"`
	Kind           string   `json:"kind"`
	Codec          string   `json:"codec"`
	Timescale      uint32   `json:"timescale"`
	Duration       int64    `json:"duration"`
	Samples        []Sample `json:"samples"`
	KeyframeGaps   []int64  `json:"keyframe_gap_ticks,omitempty"`
	KeyframeGapP50 int64    `json:"keyframe_gap_p50_ticks"`
	KeyframeGapP95 int64    `json:"keyframe_gap_p95_ticks"`
	SourceBytes    int64    `json:"source_bytes"`
	SttsEntries    int      `json:"stts_entries"`
	ConstantFrame  bool     `json:"constant_frame_rate"`
}

// Scan is the result of scanning one file.
type Scan struct {
	Path          string          `json:"path"`
	Bytes         int64           `json:"bytes"`
	Brand         string          `json:"brand,omitempty"`
	Tracks        []Track         `json:"tracks"`
	CorruptRanges []ByteRange     `json:"corrupt_ranges"`
	Faults        model.FaultList `json:"faults"`
	Truncated     bool            `json:"truncated"`
}

// ByteRange is an inclusive byte interval in the source file.
type ByteRange struct {
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	Track string `json:"track,omitempty"`
}

// Len returns the number of bytes covered.
func (b ByteRange) Len() int64 {
	if b.End < b.Start {
		return 0
	}
	return b.End - b.Start + 1
}

// Walk parses the box structure of size bytes, recording truncation instead of
// failing. It returns boxes, whether any truncation occurred, and faults.
func Walk(r io.ReaderAt, size int64) ([]box, bool, model.FaultList) {
	var faults model.FaultList
	var out []box
	var walk func(base, end uint64, path []mp4.BoxType) bool
	walk = func(base, end uint64, path []mp4.BoxType) bool {
		off := base
		for off+8 <= end {
			hdr := make([]byte, 8)
			if _, err := r.ReadAt(hdr, int64(off)); err != nil {
				faults.Add(model.Fault{
					Code: model.CodeTruncatedBox, Severity: model.SevError,
					ByteStart: int64(off), ByteEnd: int64(end - 1),
					Detail: "box header unreadable",
				})
				return false
			}
			sz := uint64(hdr[0])<<24 | uint64(hdr[1])<<16 | uint64(hdr[2])<<8 | uint64(hdr[3])
			typ := mp4.BoxType{hdr[4], hdr[5], hdr[6], hdr[7]}
			header := uint64(8)
			switch sz {
			case 1:
				if off+16 > end {
					faults.Add(model.Fault{Code: model.CodeTruncatedBox, Severity: model.SevError,
						ByteStart: int64(off), ByteEnd: int64(end - 1), Detail: "extended size header truncated"})
					return false
				}
				ext := make([]byte, 8)
				if _, err := r.ReadAt(ext, int64(off+8)); err != nil {
					return false
				}
				sz = 0
				for _, b := range ext {
					sz = sz<<8 | uint64(b)
				}
				header = 16
			case 0:
				sz = end - off
			}
			if sz < header {
				faults.Add(model.Fault{Code: model.CodeTruncatedBox, Severity: model.SevError,
					ByteStart: int64(off), ByteEnd: int64(off + header - 1),
					Detail: fmt.Sprintf("box %s declares size %d below header size", typ, sz)})
				return false
			}
			trunc := false
			if off+sz > end {
				trunc = true
				faults.Add(model.Fault{Code: model.CodeTruncatedBox, Severity: model.SevError,
					ByteStart: int64(off), ByteEnd: int64(end - 1),
					Detail: fmt.Sprintf("box %s declares %d bytes but only %d remain", typ, sz, end-off)})
				sz = end - off
			}
			childPath := append(append([]mp4.BoxType(nil), path...), typ)
			out = append(out, box{typ: typ, path: childPath, offset: off, size: sz, header: header, trunc: trunc})
			if isContainer(typ) && sz > header {
				walk(off+header, off+sz, childPath)
			}
			off += sz
			if trunc {
				return false
			}
		}
		return true
	}
	walk(0, uint64(size), nil)
	return out, len(faults) > 0, faults
}

func isContainer(t mp4.BoxType) bool {
	switch t {
	case mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(),
		mp4.BoxTypeStbl(), mp4.BoxTypeEdts(), mp4.BoxTypeDinf(), mp4.BoxTypeUdta(), mp4.BoxTypeMvex():
		return true
	}
	return false
}

// ScanFile scans one MP4 file.
func ScanFile(path, trackID string) (*Scan, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return ScanReader(f, fi.Size(), path, trackID)
}

// ScanReader scans an open file image of the given size.
func ScanReader(r io.ReaderAt, size int64, path, trackID string) (*Scan, error) {
	sc := &Scan{Path: path, Bytes: size}
	boxes, truncated, faults := Walk(r, size)
	sc.Truncated = truncated
	sc.Faults.Add(faults...)

	traks := selectBoxes(boxes, "moov/trak")
	if fts := selectBoxes(boxes, "ftyp"); len(fts) > 0 {
		var ftyp mp4.Ftyp
		if unmarshal(r, fts[0], &ftyp) == nil {
			sc.Brand = string(ftyp.MajorBrand[:])
		}
	}
	for i, trak := range traks {
		tr, err := scanTrack(r, trak, boxes, size)
		if err != nil {
			sc.Faults.Add(model.NewFault(model.CodeCorruptRange, model.SevError, "track %d: %v", i+1, err))
			continue
		}
		sc.Tracks = append(sc.Tracks, tr)
		sc.CorruptRanges = append(sc.CorruptRanges, tr.corruptRanges()...)
	}
	if len(traks) == 0 {
		sc.Faults.Add(model.NewFault(model.CodeCorruptRange, model.SevError, "%s: no readable track", path))
	}
	sc.Faults.Sort()
	return sc, nil
}

func (t Track) corruptRanges() []ByteRange {
	var out []ByteRange
	for _, s := range t.Samples {
		if s.Corrupt && s.Size > 0 {
			out = append(out, ByteRange{Start: s.Offset, End: s.Offset + int64(s.Size) - 1, Track: fmt.Sprint(t.ID)})
		}
	}
	return out
}
