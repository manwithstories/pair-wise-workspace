package archive

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"archivecheck/internal/media"
)

// SegmentStatus is the persisted state of one segment in a batch.
type SegmentStatus string

const (
	StatusPending     SegmentStatus = "pending"
	StatusRecovered   SegmentStatus = "recovered"
	StatusQuarantined SegmentStatus = "quarantined"
)

// SegmentState records enough to resume without re-emitting a sample and to
// detect a fragment whose content changed since it was committed.
type SegmentState struct {
	Segment      string        `json:"segment"`
	Status       SegmentStatus `json:"status"`
	OutputPath   string        `json:"output_path,omitempty"`
	FragmentSHA  string        `json:"fragment_sha256,omitempty"`
	SourceSHA    string        `json:"source_sha256,omitempty"`
	CacheKey     string        `json:"cache_key,omitempty"`
	Samples      int           `json:"samples"`
	Tracks       int           `json:"tracks"`
	Faults       []*media.Fault `json:"faults,omitempty"`
}

// Checkpoint is the durable, atomically-swapped batch state file.
type Checkpoint struct {
	path   string
	Batch  string                  `json:"batch"`
	Format string                  `json:"format"`
	Items  map[string]*SegmentState `json:"items"`
}

// OpenCheckpoint loads an existing checkpoint under root, or initializes one.
func OpenCheckpoint(root, batch, format string) (*Checkpoint, error) {
	cp := &Checkpoint{
		path:   filepath.Join(root, "checkpoint", batch+".json"),
		Batch:  batch,
		Format: format,
		Items:  map[string]*SegmentState{},
	}
	data, err := os.ReadFile(cp.path)
	if os.IsNotExist(err) {
		return cp, nil
	}
	if err != nil {
		return nil, media.NewFault(media.KindCheckpoint, batch, 0, -1, "read checkpoint: %s", err)
	}
	if err := json.Unmarshal(data, cp); err != nil {
		return nil, media.NewFault(media.KindCheckpoint, batch, 0, -1, "checkpoint corrupt: %s", err)
	}
	if cp.Items == nil {
		cp.Items = map[string]*SegmentState{}
	}
	return cp, nil
}

// IsDone reports whether a segment was already recovered with the same source
// digest, which makes a resume skip it and prevents duplicate ingestion.
func (cp *Checkpoint) IsDone(segment, sourceSHA string) bool {
	st, ok := cp.Items[segment]
	return ok && st.Status == StatusRecovered && st.SourceSHA == sourceSHA && st.OutputPath != ""
}

// IsQuarantined reports a previously recorded permanent failure.
func (cp *Checkpoint) IsQuarantined(segment string) bool {
	st, ok := cp.Items[segment]
	return ok && st.Status == StatusQuarantined
}

// Get returns the stored state (nil if absent).
func (cp *Checkpoint) Get(segment string) *SegmentState { return cp.Items[segment] }

// CommitRecovered records a finished fragment. It is called only after the
// output has landed on disk and its digest verification succeeded, satisfying
// the "checkpoint follows verified output" ordering. The whole file is swapped
// atomically, so an interruption cannot leave a half-written checkpoint.
func (cp *Checkpoint) CommitRecovered(st SegmentState) error {
	st.Status = StatusRecovered
	cp.Items[st.Segment] = &st
	return cp.persist()
}

// CommitQuarantined records a segment that could not be recovered, preserving
// its located faults while letting the rest of the batch proceed.
func (cp *Checkpoint) CommitQuarantined(st SegmentState) error {
	st.Status = StatusQuarantined
	cp.Items[st.Segment] = &st
	return cp.persist()
}

// Rollback drops the recorded state for one segment. Used when a re-run finds
// the previously committed fragment digest no longer matches: the stale output
// is removed first, then its checkpoint entry is cleared so it is recomputed.
func (cp *Checkpoint) Rollback(segment string) error {
	st, ok := cp.Items[segment]
	if ok && st.OutputPath != "" {
		_ = os.Remove(st.OutputPath) // best-effort; absence is the desired end state
	}
	delete(cp.Items, segment)
	return cp.persist()
}

// Snapshot returns states in stable segment order for deterministic reporting.
func (cp *Checkpoint) Snapshot() []SegmentState {
	keys := make([]string, 0, len(cp.Items))
	for k := range cp.Items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]SegmentState, 0, len(keys))
	for _, k := range keys {
		out = append(out, *cp.Items[k])
	}
	return out
}

func (cp *Checkpoint) persist() error {
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return media.NewFault(media.KindCheckpoint, cp.Batch, 0, -1, "marshal: %s", err)
	}
	if err := writeAtomic(cp.path, data); err != nil {
		return media.NewFault(media.KindCheckpoint, cp.Batch, 0, -1, "%s", err)
	}
	return nil
}
