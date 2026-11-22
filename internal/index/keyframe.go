// Package index parses and validates keyframe index files: magic/version
// header followed by fixed-width (frameSeq, offset) records. Out-of-range
// offsets are hard corruption; non-monotonic or duplicate records are
// reported as recoverable anomalies for the track mapper to resolve.
package index

import (
	"encoding/binary"
	"fmt"

	"mediapack/internal/fault"
)

const (
	// Magic and Version identify the supported index format.
	Magic   = "MPKIDX1"
	Version = uint8(1)
	// EntrySize is the fixed record width: uint64 frameSeq + uint64 offset.
	EntrySize = 16
	headerLen = len(Magic) + 1
)

// Entry is one validated keyframe record.
type Entry struct {
	FrameSeq int64
	Offset   int64
}

// Anomaly classifies a recoverable index irregularity.
type Anomaly int

const (
	AnomalyNone Anomaly = iota
	// AnomalyDuplicate: same frame sequence appears more than once.
	AnomalyDuplicate
	// AnomalyNonMonotonic: frame sequence or offset moves backwards.
	AnomalyNonMonotonic
)

func (a Anomaly) String() string {
	switch a {
	case AnomalyDuplicate:
		return "duplicate_keyframe"
	case AnomalyNonMonotonic:
		return "non_monotonic_index"
	default:
		return "none"
	}
}

// Report records one anomaly and the record position where it was seen.
type Report struct {
	Anomaly Anomaly
	Record  int
	Detail  string
}

// Parse decodes an index blob and validates every record against inputSize.
// Reports describe soft anomalies; a returned error denotes hard corruption
// (bad magic/version, truncated record, out-of-range offset).
func Parse(blob []byte, inputSize int64) ([]Entry, []Report, error) {
	if len(blob) < headerLen {
		return nil, nil, fault.At(fault.KindCorruptInput, "index", int64(len(blob)),
			"index shorter than magic/version header")
	}
	if string(blob[:len(Magic)]) != Magic {
		return nil, nil, fault.At(fault.KindCorruptInput, "index", 0,
			"bad index magic: not a mediapack keyframe index")
	}
	if v := blob[len(Magic)]; v != Version {
		return nil, nil, fault.At(fault.KindCorruptInput, "index", int64(len(Magic)),
			fmt.Sprintf("unsupported index version %d, want %d", v, Version))
	}
	body := blob[headerLen:]
	if len(body)%EntrySize != 0 {
		return nil, nil, fault.At(fault.KindCorruptInput, "index",
			int64(headerLen+len(body)-len(body)%EntrySize),
			"trailing partial index record")
	}
	n := len(body) / EntrySize
	entries := make([]Entry, 0, n)
	var reports []Report
	var prevSeq, prevOff int64 = -1, -1
	seen := make(map[int64]int)
	for i := 0; i < n; i++ {
		rec := body[i*EntrySize:]
		seq := int64(binary.BigEndian.Uint64(rec[0:8]))
		off := int64(binary.BigEndian.Uint64(rec[8:16]))
		if off < 0 || off >= inputSize {
			return nil, nil, fault.At(fault.KindCorruptInput, "index",
				int64(headerLen+i*EntrySize+8),
				fmt.Sprintf("keyframe offset %d outside input [0,%d)", off, inputSize))
		}
		if seq < 0 {
			return nil, nil, fault.At(fault.KindCorruptInput, "index",
				int64(headerLen+i*EntrySize), "negative frame sequence")
		}
		if r, ok := seen[seq]; ok {
			reports = append(reports, Report{Anomaly: AnomalyDuplicate, Record: i,
				Detail: fmt.Sprintf("frame %d first seen at record %d", seq, r)})
		}
		seen[seq] = i
		if i > 0 && (seq < prevSeq || off < prevOff) {
			reports = append(reports, Report{Anomaly: AnomalyNonMonotonic, Record: i,
				Detail: fmt.Sprintf("record %d goes backwards (seq %d->%d, off %d->%d)",
					i, prevSeq, seq, prevOff, off)})
		}
		entries = append(entries, Entry{FrameSeq: seq, Offset: off})
		prevSeq, prevOff = seq, off
	}
	return entries, reports, nil
}
