// Package provenance keeps an append-only ledger of every delivered segment
// version: which source packets it came from, which recovery operations were
// applied, its timeline version and its output digest. Entries are chained so
// a checkpointed digest can prove the ledger was not rewritten.
package provenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"example.com/medialab/archive/internal/model"
)

// Entry is one ledger line.
type Entry struct {
	Seq               int                    `json:"seq"`
	Rep               model.RepresentationID `json:"rep"`
	SegmentIndex      int                    `json:"segment_index"`
	Version           int                    `json:"version"`
	StartUS           int64                  `json:"start_us"`
	EndUS             int64                  `json:"end_us"`
	SourceStream      model.StreamID         `json:"source_stream"`
	SourceSeqMin      uint32                 `json:"source_seq_min"`
	SourceSeqMax      uint32                 `json:"source_seq_max"`
	SourcePacketCount int                    `json:"source_packet_count"`
	SourceTimes       []int64                `json:"source_times_us"`
	RecoveryOps       []string               `json:"recovery_ops,omitempty"`
	EstimatedUS       int64                  `json:"estimated_us,omitempty"`
	TimelineVersion   int                    `json:"timeline_version"`
	OutputSHA256      string                 `json:"output_sha256"`
	ReplacesVersion   int                    `json:"replaces_version,omitempty"`
	ReplacesSHA256    string                 `json:"replaces_sha256,omitempty"`
	// Quarantined lists packet ids excluded from this segment and why.
	Quarantined []model.Quarantine `json:"quarantined,omitempty"`
}

// Key identifies an idempotent ledger line.
type Key struct {
	Rep     model.RepresentationID
	Index   int
	Version int
}

// ErrConflict reports a different payload for an already-recorded version.
var ErrConflict = errors.New("provenance: conflicting ledger entry")

// Writer appends entries and maintains the chained digest. Safe for concurrent
// use; callers must preserve the intended order.
type Writer struct {
	mu    sync.Mutex
	out   io.Writer
	chain [32]byte
	count int
	seen  map[Key]string
}

// NewWriter returns a ledger writer. A nil out still maintains the digest.
func NewWriter(out io.Writer) *Writer {
	return &Writer{out: out, seen: map[Key]string{}}
}

// Restore reloads the chained digest and entry count after a checkpoint.
func (w *Writer) Restore(digest string, count int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if digest == "" {
		w.chain = [32]byte{}
	} else {
		raw, err := hex.DecodeString(digest)
		if err != nil || len(raw) != sha256.Size {
			return fmt.Errorf("provenance: invalid ledger digest %q", digest)
		}
		copy(w.chain[:], raw)
	}
	w.count = count
	return nil
}

// Remember re-registers a slot recorded in a previous process, so a resumed
// run keeps duplicate submission idempotent.
func (w *Writer) Remember(rep model.RepresentationID, index, version int, outputSHA string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen[Key{Rep: rep, Index: index, Version: version}] = outputSHA
}

// Record appends e. Repeating an identical entry is a no-op; a different
// payload for the same slot is a conflict.
func (w *Writer) Record(e Entry) (bool, error) {
	if e.Rep == "" || e.SegmentIndex < 0 || e.Version < 1 || len(e.OutputSHA256) != 64 {
		return false, fmt.Errorf("provenance: incomplete entry %+v", e)
	}
	if _, err := hex.DecodeString(e.OutputSHA256); err != nil {
		return false, fmt.Errorf("provenance: output sha is not hex: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	k := Key{Rep: e.Rep, Index: e.SegmentIndex, Version: e.Version}
	if prev, ok := w.seen[k]; ok {
		if prev == e.OutputSHA256 {
			return false, nil
		}
		return false, fmt.Errorf("%w: %s/%d v%d", ErrConflict, e.Rep, e.SegmentIndex, e.Version)
	}
	e.Seq = w.count
	line, err := json.Marshal(e)
	if err != nil {
		return false, err
	}
	if w.out != nil {
		if _, err := w.out.Write(append(line, '\n')); err != nil {
			return false, err
		}
	}
	h := sha256.New()
	h.Write(w.chain[:])
	h.Write(line)
	copy(w.chain[:], h.Sum(nil))
	w.count++
	w.seen[k] = e.OutputSHA256
	// Keep only the newest two versions of each slot in the idempotency index;
	// older versions can no longer be legitimately re-submitted.
	if e.Version > 2 {
		delete(w.seen, Key{Rep: e.Rep, Index: e.SegmentIndex, Version: e.Version - 2})
	}
	return true, nil
}

// Digest returns the chained ledger digest.
func (w *Writer) Digest() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return hex.EncodeToString(w.chain[:])
}

// Count returns the number of recorded entries.
func (w *Writer) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count
}
