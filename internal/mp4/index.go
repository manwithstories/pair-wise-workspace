// Package mp4 parses synthetic MP4 fixtures tolerantly: a truncated or
// missing sample table yields a valid prefix plus explicit index gaps
// instead of a hard failure.
package mp4

import (
	"errors"
	"io"
	"os"

	gomp4 "github.com/abema/go-mp4"

	"mediasim/internal/clock"
)

// Kind is the media role of a track, derived from hdlr.
type Kind string

const (
	KindVideo   Kind = "video"
	KindAudio   Kind = "audio"
	KindMarker  Kind = "marker"
	KindUnknown Kind = "unknown"
)

// GapKind classifies why part of a sample table cannot be trusted.
type GapKind string

const (
	GapMissingIndex   GapKind = "missing_index"
	GapTruncatedTable GapKind = "truncated_table"
	GapOutOfRange     GapKind = "out_of_range"
	GapCorruptSample  GapKind = "corrupt_sample"
	GapTableMismatch  GapKind = "table_mismatch"
)

// Gap is a byte and sample range the parser refused to trust, plus the
// recovery action it took.
type Gap struct {
	Kind     GapKind        `json:"kind"`
	TrackID  uint32         `json:"track_id"`
	Samples  [2]int         `json:"samples"` // [start, end)
	Bytes    [2]int64       `json:"bytes"`
	Recovery clock.Recovery `json:"recovery"`
	Detail   string         `json:"detail"`
}

// Sample is one entry resolved from the sample table.
type Sample struct {
	Index    int    `json:"index"`
	Offset   int64  `json:"offset"`
	Size     uint32 `json:"size"`
	DTS      int64  `json:"dts"`
	Duration int64  `json:"duration"`
	Sync     bool   `json:"sync"`
	Valid    bool   `json:"valid"`
	Reason   string `json:"reason,omitempty"`
}

// Track is one parsed track.
type Track struct {
	ID              uint32   `json:"id"`
	Kind            Kind     `json:"kind"`
	Handler         string   `json:"handler"`
	Timescale       uint32   `json:"timescale"`
	Width           int      `json:"width,omitempty"`
	Height          int      `json:"height,omitempty"`
	SampleRate      int      `json:"sample_rate,omitempty"`
	DeclaredSamples int      `json:"declared_samples"`
	Samples         []Sample `json:"samples"`
}

// ValidCount counts samples whose bytes exist and have a valid signature.
func (t Track) ValidCount() int {
	n := 0
	for _, s := range t.Samples {
		if s.Valid {
			n++
		}
	}
	return n
}

// Info is the tolerant parse result. Gaps never abort the parse.
type Info struct {
	Path        string     `json:"path"`
	Size        int64      `json:"size"`
	MoovPresent bool       `json:"moov_present"`
	Tracks      []Track    `json:"tracks"`
	Gaps        []Gap      `json:"gaps"`
	Mdat        [][2]int64 `json:"mdat"`
	Indexed     bool       `json:"indexed"`
	Synthetic   bool       `json:"synthetic"`
}

// container lists the boxes whose payload is a sequence of child boxes.
var container = map[gomp4.BoxType]bool{
	gomp4.BoxTypeMoov(): true, gomp4.BoxTypeTrak(): true, gomp4.BoxTypeMdia(): true,
	gomp4.BoxTypeMinf(): true, gomp4.BoxTypeStbl(): true,
}

type rawBox struct {
	typ       gomp4.BoxType
	off       int64
	hdr       int64
	size      int64 // declared size, may exceed available
	avail     int64 // bytes actually present from off
	truncated bool
}

func (b rawBox) payloadOff() int64 { return b.off + b.hdr }
func (b rawBox) payloadAvail() int64 {
	n := b.avail - b.hdr
	if n < 0 {
		return 0
	}
	return n
}

// walkBoxes enumerates boxes in [start,end) without ever failing on a short
// file: a cut header or oversized box becomes a truncated entry.
func walkBoxes(f *os.File, start, end int64, fn func(path []gomp4.BoxType, offs []int64, b rawBox)) error {
	var rec func(start, end int64, path []gomp4.BoxType, offs []int64) error
	rec = func(start, end int64, path []gomp4.BoxType, offs []int64) error {
		pos := start
		for pos < end {
			if _, err := f.Seek(pos, io.SeekStart); err != nil {
				return err
			}
			bi, err := gomp4.ReadBoxInfo(f)
			if err != nil {
				// A cut header leaves fewer than 8 bytes: nothing to record.
				return nil
			}
			size := int64(bi.Size)
			hdr := int64(bi.HeaderSize)
			avail := end - pos
			truncated := size > avail
			if size < hdr {
				truncated = true
			}
			b := rawBox{typ: bi.Type, off: pos, hdr: hdr, size: size, avail: min64(size, avail), truncated: truncated}
			curPath := append(append([]gomp4.BoxType(nil), path...), bi.Type)
			curOffs := append(append([]int64(nil), offs...), pos)
			fn(curPath, curOffs, b)
			if container[bi.Type] && b.payloadAvail() > 0 {
				if err := rec(b.payloadOff(), b.payloadOff()+b.payloadAvail(), curPath, curOffs); err != nil {
					return err
				}
			}
			if truncated {
				return nil
			}
			pos += size
		}
		return nil
	}
	return rec(start, end, nil, nil)
}

// readPayload returns up to n present payload bytes of b.
func readPayload(f *os.File, b rawBox) ([]byte, error) {
	n := min64(b.payloadAvail(), b.size-b.hdr)
	if n <= 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, b.payloadOff()); err != nil {
		if errors.Is(err, io.EOF) {
			return buf, nil
		}
		return nil, err
	}
	return buf, nil
}
