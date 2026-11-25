package index

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// IndexVersion is the canonical schema version.
const IndexVersion = 1

// SampleBoundary pins one sample to its common-clock time and its byte
// location inside the repaired chunk. Random-access seeks resolve to
// exactly these boundaries.
type SampleBoundary struct {
	SampleIndex int    `json:"sample_index"`
	DTSCommon   int64  `json:"dts_common"`
	PTSCommon   int64  `json:"pts_common"`
	Duration    int64  `json:"duration_common"`
	Size        uint32 `json:"size"`
	Sync        bool   `json:"sync"`
	ByteOffset  int64  `json:"byte_offset"`
}

// Chunk is one atomically written, randomly addressable fragment.
type Chunk struct {
	ID                string           `json:"id"`
	TrackID           string           `json:"track_id"`
	SegmentIndex      int              `json:"segment_index"`
	SourceRel         string           `json:"source_rel"`
	OutputRel         string           `json:"output_rel"`
	Timescale         uint32           `json:"timescale"`
	CorrectedBase     int64            `json:"corrected_base_native"`
	StartCommon       int64            `json:"start_common"`
	EndCommon         int64            `json:"end_common"`
	OutputSize        int64            `json:"output_size"`
	OutputSHA256      string           `json:"output_sha256"`
	SampleCount       int              `json:"sample_count"`
	SyncSampleIndexes []int            `json:"sync_sample_indexes,omitempty"`
	Samples           []SampleBoundary `json:"samples,omitempty"`
}

// Breakpoint is the index-level projection of a timeline boundary.
type Breakpoint struct {
	TrackID      string `json:"track_id"`
	SegmentIndex int    `json:"segment_index"`
	Kind         string `json:"kind"`
}

// RTPSummary records packet-loss recovery in the index so that loss
// outcomes propagate into IndexSHA256.
type RTPSummary struct {
	Stream          string  `json:"stream"`
	TotalLost       int     `json:"total_lost"`
	Recovered       int     `json:"recovered"`
	Duplicates      int     `json:"duplicates"`
	Reordered       int     `json:"reordered"`
	UnrecoveredGaps int     `json:"unrecovered_gaps"`
	LossRecovery    float64 `json:"loss_recovery"`
}

// MetadataConflict pins a track whose metadata could not be uniquely
// reconciled.
type MetadataConflict struct {
	TrackID string `json:"track_id"`
	Detail  string `json:"detail"`
}

// Index is the deterministic chunk index. It deliberately contains no
// wall-clock timestamps or random map ordering so repeated runs on the
// same inputs produce an identical canonical digest.
type Index struct {
	Version           int                `json:"version"`
	InputSHA256       string             `json:"input_sha256"`
	CommonTimescale   uint32             `json:"common_timescale"`
	Chunks            []Chunk            `json:"chunks"`
	Breakpoints       []Breakpoint       `json:"breakpoints,omitempty"`
	RTP               []RTPSummary       `json:"rtp,omitempty"`
	MetadataConflicts []MetadataConflict `json:"metadata_conflicts,omitempty"`
	UnrecoveredGaps   int                `json:"unrecovered_gaps"`
}

// Canonical returns the deterministic JSON encoding.
func (idx *Index) Canonical() ([]byte, error) {
	return json.MarshalIndent(idx, "", "  ")
}

// Digest returns the 64-hex SHA-256 of the canonical index bytes.
func (idx *Index) Digest() (string, error) {
	b, err := idx.Canonical()
	if err != nil {
		return "", err
	}
	return SHA256Bytes(b), nil
}

// SaveAtomic writes the canonical index to path via a same-directory
// temp file plus atomic rename.
func (idx *Index) SaveAtomic(path string) (digest string, err error) {
	b, err := idx.Canonical()
	if err != nil {
		return "", err
	}
	if err := WriteFileAtomic(path, b, 0o644); err != nil {
		return "", err
	}
	return SHA256Bytes(b), nil
}

// Load reads an index file.
func Load(path string) (*Index, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

// WriteFileAtomic writes data to path.tmp-* in the same directory,
// fsyncs and renames over path. Readers therefore only ever observe a
// complete file.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if mode != 0 {
		_ = os.Chmod(tmpName, mode)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
