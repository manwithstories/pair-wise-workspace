// Package segment builds the delivery manifest. Entries are appended in a
// canonical order and hashed as a chain, so the archive digest is incremental,
// restorable from a checkpoint and independent of duplicate submissions.
package segment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"example.com/medialab/archive/internal/model"
)

// Entry is one manifest line: one version of one aligned segment.
type Entry struct {
	Rep               model.RepresentationID `json:"rep"`
	Index             int                    `json:"index"`
	Version           int                    `json:"version"`
	StartUS           int64                  `json:"start_us"`
	EndUS             int64                  `json:"end_us"`
	FrameCount        int                    `json:"frame_count"`
	SourceStream      model.StreamID         `json:"source_stream"`
	SourceSeqMin      uint32                 `json:"source_seq_min"`
	SourceSeqMax      uint32                 `json:"source_seq_max"`
	SourcePacketCount int                    `json:"source_packet_count"`
	SourceStartUS     int64                  `json:"source_start_us"`
	SourceEndUS       int64                  `json:"source_end_us"`
	BoundarySkewUS    int64                  `json:"boundary_skew_us"`
	SilenceUS         int64                  `json:"silence_us"`
	RecoveredPackets  int                    `json:"recovered_packets"`
	RecoveryOps       []string               `json:"recovery_ops,omitempty"`
	OutputSHA256      string                 `json:"output_sha256"`
	ReplacesVersion   int                    `json:"replaces_version,omitempty"`
	ReplacesSHA256    string                 `json:"replaces_sha256,omitempty"`
	// Reason and cause identify a replay: which packet forced recalculation
	// and at what event time, so the replacement edge is auditable.
	Reason           string `json:"reason,omitempty"`
	CauseSeq         uint32 `json:"cause_seq,omitempty"`
	CauseTimeUS      int64  `json:"cause_time_us,omitempty"`
	QuarantinedCount int    `json:"quarantined_count,omitempty"`
	// MediaSize and InitSHA256 describe the written fMP4 artefacts when the
	// run was asked to materialise them.
	MediaSize  int64  `json:"media_size,omitempty"`
	InitSHA256 string `json:"init_sha256,omitempty"`
	// Hole accounting for this window. Every hole is attributed to the window
	// containing its start, so totals stay exact across replays.
	LossFills         int `json:"loss_fills,omitempty"`
	CorruptFills      int `json:"corrupt_fills,omitempty"`
	UnrecoverableGaps int `json:"unrecoverable_gaps,omitempty"`
}

// Key is the uniqueness key of a manifest entry.
type Key struct {
	Rep     model.RepresentationID
	Index   int
	Version int
}

// ErrVersionConflict reports a second, different payload for the same slot and
// version. Committing it would make the archive non-reproducible.
var ErrVersionConflict = errors.New("segment: conflicting re-commit for the same version")

// ErrVersionGap reports a version that does not follow the committed one.
var ErrVersionGap = errors.New("segment: version does not follow the committed version")

// Writer appends manifest entries to an io.Writer and maintains the chained
// digest. It is safe for concurrent use.
type Writer struct {
	mu        sync.Mutex
	out       io.Writer
	chain     [32]byte
	count     int
	commits   int
	latest    map[model.RepresentationID]map[int]int
	latestSHA map[model.RepresentationID]map[int]string
}

// NewWriter creates a writer. A nil out discards entries while still tracking
// the digest (useful for verification runs).
func NewWriter(out io.Writer) *Writer {
	return &Writer{
		out:       out,
		latest:    map[model.RepresentationID]map[int]int{},
		latestSHA: map[model.RepresentationID]map[int]string{},
	}
}

// Restore reloads the chained digest and entry count from a checkpoint. The
// per-slot version index is rebuilt lazily from subsequent commits; callers
// that resume at a known cursor should also re-register the last committed
// per-rep indices via SetCursor.
func (w *Writer) Restore(chainHex string, count int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if chainHex == "" {
		w.chain = [32]byte{}
	} else {
		raw, err := hex.DecodeString(chainHex)
		if err != nil || len(raw) != sha256.Size {
			return fmt.Errorf("segment: invalid manifest digest %q", chainHex)
		}
		copy(w.chain[:], raw)
	}
	w.count = count
	return nil
}

// SetCursor records the highest committed index and digests per rep after a
// resume, so duplicate and replay submissions stay idempotent across restarts.
func (w *Writer) SetCursor(rep model.RepresentationID, index, version int, sha string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.latest[rep] == nil {
		w.latest[rep] = map[int]int{}
		w.latestSHA[rep] = map[int]string{}
	}
	w.latest[rep][index] = version
	w.latestSHA[rep][index] = sha
}

// Commit appends e if it advances its slot. A repeated submission of the exact
// same content is ignored and returns false. A conflicting payload for the same
// version is rejected.
func (w *Writer) Commit(e Entry) (bool, error) {
	if err := validate(e); err != nil {
		return false, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.latest[e.Rep] == nil {
		w.latest[e.Rep] = map[int]int{}
		w.latestSHA[e.Rep] = map[int]string{}
	}
	cur := w.latest[e.Rep][e.Index]
	curSHA := w.latestSHA[e.Rep][e.Index]
	if e.Version == cur {
		if e.OutputSHA256 == curSHA {
			return false, nil // idempotent duplicate submission
		}
		return false, fmt.Errorf("%w: %s/%d v%d", ErrVersionConflict, e.Rep, e.Index, e.Version)
	}
	if cur != 0 && e.Version != cur+1 {
		return false, fmt.Errorf("%w: %s/%d v%d after v%d", ErrVersionGap, e.Rep, e.Index, e.Version, cur)
	}
	if cur == 0 && e.Version != 1 {
		return false, fmt.Errorf("%w: %s/%d starts at v%d", ErrVersionGap, e.Rep, e.Index, e.Version)
	}
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
	w.commits++
	w.latest[e.Rep][e.Index] = e.Version
	w.latestSHA[e.Rep][e.Index] = e.OutputSHA256
	// Bounded bookkeeping: segments two or more slots behind the newest one
	// for this rep can no longer be replayed or re-submitted.
	cut := e.Index - 1
	for idx := range w.latest[e.Rep] {
		if idx < cut {
			delete(w.latest[e.Rep], idx)
			delete(w.latestSHA[e.Rep], idx)
		}
	}
	return true, nil
}

// ChainHex returns the chained manifest digest.
func (w *Writer) ChainHex() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return hex.EncodeToString(w.chain[:])
}

// Count returns the number of committed entries.
func (w *Writer) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count
}

func validate(e Entry) error {
	if e.Rep == "" {
		return errors.New("segment: missing representation")
	}
	if e.Index < 0 || e.Version < 1 {
		return fmt.Errorf("segment: invalid index/version %d/%d", e.Index, e.Version)
	}
	if e.EndUS <= e.StartUS {
		return fmt.Errorf("segment: non-positive range %d-%d", e.StartUS, e.EndUS)
	}
	if len(e.OutputSHA256) != 64 {
		return fmt.Errorf("segment: output sha must be 64 hex chars, got %d", len(e.OutputSHA256))
	}
	if _, err := hex.DecodeString(e.OutputSHA256); err != nil {
		return fmt.Errorf("segment: output sha is not hex: %w", err)
	}
	if e.Version > 1 && e.ReplacesVersion != e.Version-1 {
		return fmt.Errorf("segment: replay v%d must replace v%d", e.Version, e.Version-1)
	}
	return nil
}

// SortEntries orders entries canonically by rep, index and version. Used for
// verification when an external consumer re-reads a manifest file.
func SortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Rep != b.Rep {
			return a.Rep < b.Rep
		}
		if a.Index != b.Index {
			return a.Index < b.Index
		}
		return a.Version < b.Version
	})
}
