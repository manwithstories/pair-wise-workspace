// Package index scans MP4/MOV containers, validates atom and sample
// boundaries, and persists per-input indexes keyed by the source SHA-256.
package index

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"mediad/internal/model"
)

// AtomRec records one raw container atom discovered by the boundary walker.
type AtomRec struct {
	Type   string `json:"type"`
	Offset int64  `json:"offset"`
	Header int64  `json:"header"`
	Size   int64  `json:"size"`
	Depth  int    `json:"depth"`
}

// Sample is one indexed sample located on the unified nanosecond base.
type Sample struct {
	Index      uint32          `json:"index"`
	Offset     int64           `json:"offset"`
	Size       uint32          `json:"size"`
	DTS        model.Timestamp `json:"dts_ns"`
	PTS        model.Timestamp `json:"pts_ns"`
	IsKeyframe bool            `json:"is_keyframe"`
}

// Track is one parsed media track.
type Track struct {
	TrackID    uint32   `json:"track_id"`
	Handler    string   `json:"handler"`
	Codec      string   `json:"codec"`
	Timescale  uint32   `json:"timescale"`
	IsVideo    bool     `json:"is_video"`
	IsAudio    bool     `json:"is_audio"`
	Samples    []Sample `json:"samples"`
	FirstPTS   int64    `json:"first_pts_ns"`
	Regression int      `json:"pts_regression_at"` // sample index, -1 when clean
	WrappedAt  int      `json:"pts_wrap_at"`       // sample index, -1 when clean
}

// FileIndex is the complete, content-addressed scan result of one file.
type FileIndex struct {
	Path         string         `json:"path"`
	Name         string         `json:"name"`
	Size         int64          `json:"size"`
	SHA256       string         `json:"sha256"`
	MoovBefore   bool           `json:"moov_before_mdat"`
	MdatOffset   int64          `json:"mdat_offset"`
	MdatPayload  int64          `json:"mdat_payload_offset"`
	MdatSize     int64          `json:"mdat_size"`
	Atoms        []AtomRec      `json:"atoms"`
	Tracks       []Track        `json:"tracks"`
	Faults       []*model.Fault `json:"faults"`
	FirstBadByte int64          `json:"first_bad_offset"`
}

// ScanResult bundles the index with the (possibly non-nil) hard scan error.
// A non-nil error does not invalidate the partial index embedded in it.
type ScanResult struct {
	Index *FileIndex
	Err   error
}

// container atoms whose children the walker descends into.
var containers = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true,
	"stbl": true, "udta": true, "edts": true,
}

// walkAtoms performs a raw, allocation-bounded atom walk. Every declared
// size is validated against fileSize before any descent, so a truncated or
// out-of-bounds atom yields a Fault whose offset is the exact first bad
// byte. No atom larger than model.MaxAtomSize is ever traversed.
func walkAtoms(r io.ReadSeeker, start, end int64, depth int, out *[]AtomRec) error {
	pos := start
	header := make([]byte, 8)
	for pos < end {
		if end-pos < 8 {
			return model.NewFault(model.FaultTruncatedAtom, pos, "",
				fmt.Sprintf("atom header truncated: %d of 8 bytes remain", end-pos))
		}
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.ReadFull(r, header); err != nil {
			return model.NewFault(model.FaultTruncatedAtom, pos, "", "cannot read atom header")
		}
		size := int64(uint64(header[0])<<24 | uint64(header[1])<<16 |
			uint64(header[2])<<8 | uint64(header[3]))
		typ := string(header[4:8])
		hdr := int64(8)
		switch size {
		case 1: // 64-bit largesize
			ext := make([]byte, 8)
			if end-pos-8 < 8 {
				return model.NewFault(model.FaultTruncatedAtom, pos+8,
					"", "64-bit largesize truncated")
			}
			if _, err := io.ReadFull(r, ext); err != nil {
				return model.NewFault(model.FaultTruncatedAtom, pos+8, "", "cannot read largesize")
			}
			size = int64(binary.BigEndian.Uint64(ext))
			hdr = 16
		case 0: // box extends to end of file
			size = end - pos
		}
		if size < hdr {
			return model.NewFault(model.FaultContainerBounds, pos, typ,
				fmt.Sprintf("declared size %d smaller than header %d", size, hdr))
		}
		if pos+size > end {
			// The atom's declared region runs past the container: the first
			// byte the file cannot provide sits exactly at the truncation point.
			return model.NewFault(model.FaultTruncatedAtom, end,
				typ, fmt.Sprintf("atom declares %d bytes but %d remain", size, end-pos))
		}
		if size > model.MaxAtomSize {
			return model.NewFault(model.FaultOutOfBounds, pos, typ,
				fmt.Sprintf("atom size %d exceeds limit %d", size, model.MaxAtomSize))
		}
		*out = append(*out, AtomRec{Type: typ, Offset: pos, Header: hdr, Size: size, Depth: depth})
		if depth < 4 && containers[typ] {
			if err := walkAtoms(r, pos+hdr, pos+size, depth+1, out); err != nil {
				return err
			}
		}
		pos += size
	}
	return nil
}

// Scan opens path, walks its atoms, parses track tables and validates every
// sample against the mdat bounds. The returned ScanResult always carries a
// usable FileIndex even when Err is non-nil.
func Scan(path string) (*ScanResult, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	digest, err := HashFile(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	idx := &FileIndex{Path: path, Name: st.Name(), Size: st.Size(), SHA256: digest,
		FirstBadByte: model.OffsetUnknown}
	addFault := func(fe *model.Fault) {
		fe.Source = idx.Name
		idx.Faults = append(idx.Faults, fe)
		if idx.FirstBadByte == model.OffsetUnknown ||
			(fe.Offset >= 0 && fe.Offset < idx.FirstBadByte) {
			idx.FirstBadByte = fe.Offset
		}
	}

	walkErr := walkAtoms(f, 0, st.Size(), 0, &idx.Atoms)
	if fe, ok := model.AsFault(walkErr); ok {
		addFault(fe)
	} else if walkErr != nil {
		return &ScanResult{Index: idx}, walkErr
	}

	var moov, mdat *AtomRec
	var mdatOrdinal, moovOrdinal int
	for i := range idx.Atoms {
		a := &idx.Atoms[i]
		switch a.Type {
		case "moov":
			moov = a
			moovOrdinal = i
		case "mdat":
			mdat = a
			mdatOrdinal = i
		}
	}
	if mdat != nil {
		idx.MdatOffset = mdat.Offset
		idx.MdatPayload = mdat.Offset + mdat.Header
		idx.MdatSize = mdat.Size
	}
	idx.MoovBefore = moov != nil && mdat != nil && moovOrdinal < mdatOrdinal

	if moov == nil {
		addFault(model.NewFault(model.FaultMissingIndex, model.OffsetUnknown,
			idx.Name, "moov atom missing: no sample index present"))
		return &ScanResult{Index: idx}, walkErr
	}
	if err := parseTracks(f, idx, moov, addFault); err != nil {
		if fe, ok := model.AsFault(err); ok {
			addFault(fe)
		} else {
			return &ScanResult{Index: idx}, err
		}
	}
	return &ScanResult{Index: idx}, walkErr
}
