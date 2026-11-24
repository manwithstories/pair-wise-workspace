// Package report writes newline-delimited JSON run records. One object per
// line keeps large runs streamable; a deterministic key order and a trailing
// content hash make reruns comparable.
package report

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Record is one JSONL object. Field order follows the acceptance contract so
// the emitted lines are self-describing without a separate schema.
type Record struct {
	Case            string             `json:"case"`
	Representations int                `json:"representations"`
	Segments        int                `json:"segments"`
	TargetBPS       int64              `json:"target_bps"`
	ActualBPS       int64              `json:"actual_bps"`
	BitrateDeltaPct float64            `json:"bitrate_delta_pct"`
	ValidSamplePct  float64            `json:"valid_sample_pct"`
	GOPP95Ms        float64            `json:"gop_p95_ms"`
	DamagedRanges   []DamagedRangeJSON `json:"damaged_ranges"`
	AlignmentMaxMs  int64              `json:"alignment_max_ms"`
	RetryReused     int                `json:"retry_reused"`
	FirstBlocked    string             `json:"first_blocked,omitempty"`
	ReportSHA256    string             `json:"report_sha256"`
}

// DamagedRangeJSON is the stable JSON shape of one damaged span.
type DamagedRangeJSON struct {
	StartMs int64  `json:"start_ms"`
	EndMs   int64  `json:"end_ms"`
	Kind    string `json:"kind"`
	RepID   string `json:"rep_id"`
	Segment int    `json:"segment"`
	Line    int    `json:"line,omitempty"`
}

// Writer appends records and maintains a running hash of everything written,
// excluding the per-record hash field itself.
type Writer struct {
	bw      *bufio.Writer
	f       *os.File
	hasher  hash.Hash
	records int
}

// NewWriter creates (or truncates) a JSONL report file.
func NewWriter(path string) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &Writer{
		bw:     bufio.NewWriterSize(f, 1<<16),
		f:      f,
		hasher: sha256.New(),
	}, nil
}

// Append writes one record and returns the stored copy, which has the
// ReportSHA256 field populated. The self-hash and running content hash are
// computed from a canonical encoding that excludes both the self-hash and the
// runtime-only RetryReused counter, so a resumed run (which reuses artifacts)
// hashes identically to a fresh run over the same media.
func (w *Writer) Append(r Record) (Record, error) {
	content := r
	content.ReportSHA256 = ""
	content.RetryReused = 0
	canonical, err := json.Marshal(content)
	if err != nil {
		return r, err
	}
	sum := sha256.Sum256(canonical)
	r.ReportSHA256 = hex.EncodeToString(sum[:])
	line, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	// Hash the canonical, runtime-field-free line plus newline so the digest
	// depends only on the media verdict, not on scheduler reuse counters.
	if _, err := w.hasher.Write(append(canonical, '\n')); err != nil {
		return r, err
	}
	if _, err := w.bw.Write(append(line, '\n')); err != nil {
		return r, err
	}
	w.records++
	return r, nil
}

// Close flushes buffers and closes the file.
func (w *Writer) Close() error {
	if err := w.bw.Flush(); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// ContentHash returns the running digest over canonical lines.
func (w *Writer) ContentHash() string {
	return hex.EncodeToString(w.hasher.Sum(nil))
}

// Records returns the number appended so far.
func (w *Writer) Records() int { return w.records }

// ReadAll loads JSONL records for verification in tests.
func ReadAll(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(f, 1<<16))
	var out []Record
	for {
		var r Record
		if err := dec.Decode(&r); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// SortDamagedRanges imposes deterministic order before serialization.
func SortDamagedRanges(ranges []DamagedRangeJSON) {
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].RepID != ranges[j].RepID {
			return ranges[i].RepID < ranges[j].RepID
		}
		if ranges[i].StartMs != ranges[j].StartMs {
			return ranges[i].StartMs < ranges[j].StartMs
		}
		return ranges[i].Kind < ranges[j].Kind
	})
}
