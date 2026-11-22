package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"mediarun/internal/mediaerr"
	"mediarun/internal/metadata"
)

// Span is a half-open byte range [Start, End).
type Span struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// TraceEntry records the provenance of one output sample: which input byte
// range and track it came from, and where it landed in the output.
type TraceEntry struct {
	TrackID    string `json:"track_id"`
	SampleIdx  int64  `json:"sample_index"`
	InputSpan  Span   `json:"input_span"`
	OutputSpan Span   `json:"output_span"`
	PTS        int64  `json:"pts_ticks"`
	DTS        int64  `json:"dts_ticks"`
}

// Manifest is the full provenance document committed atomically with output.
// It deliberately stores only media-derived identifiers, never host paths
// beyond the sandboxed work directory or any environment-sensitive data.
type Manifest struct {
	InputHash   string                         `json:"input_hash"`
	MappingHash string                         `json:"mapping_hash"`
	OutputHash  string                         `json:"output_hash"`
	Mappings    []map[string]string            `json:"track_mappings"`
	Decisions   map[string][]metadata.Decision `json:"metadata_decisions"`
	Entries     []TraceEntry                   `json:"entries"`
}

// Trace accumulates provenance entries in a thread-safe way. Entries are
// appended as samples are remuxed; the manifest is serialized once at commit.
type Trace struct {
	mu        sync.Mutex
	manifest  Manifest
	outCursor map[string]int64 // running output offset per track
}

// NewTrace initializes an empty provenance builder.
func NewTrace(inputHash, mappingHash string, mappings []map[string]string,
	decisions map[string][]metadata.Decision) *Trace {
	return &Trace{
		manifest: Manifest{
			InputHash:   inputHash,
			MappingHash: mappingHash,
			Mappings:    mappings,
			Decisions:   decisions,
			Entries:     make([]TraceEntry, 0),
		},
		outCursor: map[string]int64{},
	}
}

// Record appends one sample's provenance and advances the output cursor.
func (t *Trace) Record(ev TraceEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.manifest.Entries = append(t.manifest.Entries, ev)
}

// NextOutputSpan returns the current output offset for a track and advances
// it by size, so consecutive samples get contiguous, non-overlapping spans.
func (t *Trace) NextOutputSpan(trackID string, size int64) Span {
	t.mu.Lock()
	defer t.mu.Unlock()
	start := t.outCursor[trackID]
	t.outCursor[trackID] = start + size
	return Span{Start: start, End: start + size}
}

// Finalize stamps the output hash, sorts entries for deterministic ordering,
// and atomically writes the manifest next to the committed output.
func (t *Trace) Finalize(workDir, name, outputHash string) (string, error) {
	t.mu.Lock()
	t.manifest.OutputHash = outputHash
	sort.SliceStable(t.manifest.Entries, func(i, j int) bool {
		a, b := t.manifest.Entries[i], t.manifest.Entries[j]
		if a.TrackID != b.TrackID {
			return a.TrackID < b.TrackID
		}
		return a.SampleIdx < b.SampleIdx
	})
	raw, err := json.MarshalIndent(t.manifest, "", "  ")
	t.mu.Unlock()
	if err != nil {
		return "", mediaerr.New(mediaerr.CodeIO, err.Error())
	}
	final := filepath.Join(workDir, name)
	tmp, err := os.CreateTemp(workDir, ".manifest.tmp-*")
	if err != nil {
		return "", mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(final)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(final)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(final)
	}
	if err := tmp.Close(); err != nil {
		return "", mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(final)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(final)
	}
	return final, nil
}

// Count returns how many provenance entries have been recorded.
func (t *Trace) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.manifest.Entries)
}
