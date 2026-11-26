package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"context"

	"aircheck/internal/media"
)

// Processor is the transform stage: demux, optionally remux, and compute the
// hashes the verifier checks. Implementations must be safe for one goroutine
// at a time; the runner serialises calls so output stays deterministic.
type Processor interface {
	Process(ctx context.Context, in SegmentData) (Processed, error)
}

// SegmentData is one whole segment handed to the processor.
type SegmentData struct {
	Index    int
	Seq      int64
	URI      string
	Duration media.Duration
	Data     []byte
	// Resume marks a segment whose committed output already exists; the
	// processor should only re-verify, not rewrite.
	Resume bool
}

// Processed is the processor's verdict and its materialised output.
type Processed struct {
	PayloadHash  string
	OutputHash   string
	Output       []byte // remuxed bytes; nil on a resume pass
	Frames       int
	Keyframes    int
	AudioSamples int
	TrackTimes   map[media.Kind]int64
}

// Outcome is the per-segment result reported to the demo.
type Outcome struct {
	Index        int    `json:"index"`
	Seq          int64  `json:"seq"`
	Committed    bool   `json:"committed"`
	Quarantined  bool   `json:"quarantined"`
	Skipped      bool   `json:"skipped"`
	Fault        string `json:"fault,omitempty"`
	FaultKind    string `json:"fault_kind,omitempty"`
	SourceOffset int64  `json:"source_offset"`
	PayloadHash  string `json:"payload_sha"`
	OutputHash   string `json:"output_sha,omitempty"`
	Frames       int    `json:"frames"`
	Keyframes    int    `json:"keyframes"`
	AudioSamples int    `json:"audio_samples"`
	OutputBytes  int64  `json:"output_bytes"`

	outputData []byte // unexported: not part of the JSON report
}

// Report is the run-level result.
type Report struct {
	Outcomes     []Outcome
	Stats        Stats
	Cancelled    bool
	ResumedParts int
	ElapsedMs    int64
	// Fatal holds the first unrecoverable error encountered while processing;
	// it does not stop already-commit-safe work but must fail the run.
	Fatal error

	mu sync.Mutex
}

// Committed reports whether every outcome is committed or a resume hit.
func (r *Report) Committed() bool {
	for _, o := range r.Outcomes {
		if o.Quarantined {
			continue
		}
		if !o.Committed && !o.Skipped {
			return false
		}
	}
	return true
}

// Quarantined returns the isolated outcomes.
func (r *Report) Quarantined() []Outcome {
	var out []Outcome
	for _, o := range r.Outcomes {
		if o.Quarantined {
			out = append(out, o)
		}
	}
	return out
}

// RunnerOptions configures the bounded pipeline.
type RunnerOptions struct {
	BlockBytes     int64
	MaxBytes       int64
	SlowConsumerMs int64 // artificial consumer delay, for backpressure demos
	OutDir         string
	quarantineDir  string
}

// Store is the atomic commit area. A committed segment is a file that exists
// under OutDir plus a ledger entry appended only after the rename succeeded.
// Half-written segments therefore never appear in the ledger the manifest
// writer consumes.
type Store struct {
	dir        string
	quarantine string
	mu         sync.Mutex
	ledger     map[int64]LedgerEntry
}

// LedgerEntry records one committed segment.
type LedgerEntry struct {
	Seq         int64  `json:"seq"`
	Index       int    `json:"index"`
	OutputHash  string `json:"output_sha"`
	PayloadHash string `json:"payload_sha"`
	Bytes       int64  `json:"bytes"`
}

// OpenStore loads an existing ledger so a rerun can skip committed segments.
func OpenStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("pipeline: store directory required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, media.WrapFault(media.FaultIO, media.SourcePos{Artifact: dir}, err)
	}
	s := &Store{dir: dir, quarantine: filepath.Join(dir, "quarantine"), ledger: map[int64]LedgerEntry{}}
	data, err := os.ReadFile(filepath.Join(dir, "ledger.json"))
	if err == nil {
		var entries []LedgerEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, media.WrapFault(media.FaultStateInvariant, media.SourcePos{Artifact: "ledger.json"}, err)
		}
		for _, e := range entries {
			s.ledger[e.Seq] = e
		}
	} else if !os.IsNotExist(err) {
		return nil, media.WrapFault(media.FaultIO, media.SourcePos{Artifact: dir}, err)
	}
	return s, nil
}

// Has reports whether the segment was committed by an earlier run.
func (s *Store) Has(seq int64) (LedgerEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.ledger[seq]
	return e, ok
}

// filename for a segment's committed payload.
func segFile(seq int64, index int) string { return fmt.Sprintf("seg-%06d-%05d.m4s", seq, index) }

// Commit writes output atomically: a temporary file is fsynced and renamed, and
// only then does the ledger gain an entry. Any failure before the rename leaves
// no committed product.
func (s *Store) Commit(o Outcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := segFile(o.Seq, o.Index)
	final := filepath.Join(s.dir, name)
	tmp, err := os.CreateTemp(s.dir, ".part-*")
	if err != nil {
		return media.WrapFault(media.FaultIO, media.SourcePos{Artifact: name}, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(o.outputData); err != nil {
		tmp.Close()
		cleanup()
		return media.WrapFault(media.FaultIO, media.SourcePos{Artifact: name}, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return media.WrapFault(media.FaultIO, media.SourcePos{Artifact: name}, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return media.WrapFault(media.FaultIO, media.SourcePos{Artifact: name}, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		cleanup()
		return media.WrapFault(media.FaultIO, media.SourcePos{Artifact: name}, err)
	}
	e := LedgerEntry{Seq: o.Seq, Index: o.Index, OutputHash: o.OutputHash, PayloadHash: o.PayloadHash, Bytes: o.OutputBytes}
	s.ledger[o.Seq] = e
	return s.writeLedger()
}

// writeLedger rewrites the ledger atomically; the caller holds s.mu.
func (s *Store) writeLedger() error {
	entries := make([]LedgerEntry, 0, len(s.ledger))
	for _, e := range s.ledger {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Seq < entries[j].Seq })
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return media.WrapFault(media.FaultStateInvariant, media.SourcePos{Artifact: "ledger.json"}, err)
	}
	path := filepath.Join(s.dir, "ledger.json")
	tmp, err := os.CreateTemp(s.dir, ".ledger-*")
	if err != nil {
		return media.WrapFault(media.FaultIO, media.SourcePos{Artifact: path}, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return media.WrapFault(media.FaultIO, media.SourcePos{Artifact: path}, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return media.WrapFault(media.FaultIO, media.SourcePos{Artifact: path}, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return media.WrapFault(media.FaultIO, media.SourcePos{Artifact: path}, err)
	}
	return nil
}

// Quarantine isolates a damaged segment's original bytes under a dedicated
// directory with its fault recorded, so the rest of the run continues.
func (s *Store) Quarantine(o Outcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.quarantine, 0o755); err != nil {
		return media.WrapFault(media.FaultIO, media.SourcePos{Artifact: s.quarantine}, err)
	}
	note := fmt.Sprintf("index=%d seq=%d kind=%s offset=%d fault=%s\n",
		o.Index, o.Seq, o.FaultKind, o.SourceOffset, o.Fault)
	name := filepath.Join(s.quarantine, fmt.Sprintf("%s.note", segFile(o.Seq, o.Index)))
	if err := os.WriteFile(name, []byte(note), 0o644); err != nil {
		return media.WrapFault(media.FaultIO, media.SourcePos{Artifact: name}, err)
	}
	return nil
}

// SafeJoin resolves a root-relative manifest path, refusing anything that
// escapes the input root. Kept beside the store because both enforce the same
// containment rule.
func SafeJoin(root, rel string) (string, error) {
	if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return "", fmt.Errorf("%w: path %q escapes root", media.FaultManifestRange, rel)
	}
	clean := filepath.Clean(filepath.Join(root, rel))
	rootClean := filepath.Clean(root)
	if clean != rootClean && !strings.HasPrefix(clean, rootClean+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path %q escapes root", media.FaultManifestRange, rel)
	}
	return clean, nil
}

// CommitOutcome atomically stores the processor output described by o and then
// records it in the ledger.
func (s *Store) CommitOutcome(o Outcome, data []byte) error {
	o.outputData = data
	return s.Commit(o)
}

// SleepFor lets a test or demo inject consumer latency deterministically.
func SleepFor(ctx context.Context, ms int64) error {
	if ms <= 0 {
		return nil
	}
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Sha256Hex is the shared digest helper.
func Sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
