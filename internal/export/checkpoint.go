package export

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"medpipe/internal/fault"
)

// Metrics is the run summary printed by every code path.
type Metrics struct {
	PacketsTotal     int     `json:"packets_total"`
	PacketsLost      int     `json:"packets_lost"`
	BadOffsetBytes   int64   `json:"bad_offset_bytes"`
	FramesWritten    int     `json:"frames_written"`
	ResumeSeconds    float64 `json:"resume_seconds"`
	OutputSHA256     string  `json:"output_sha256"`
	ProvenanceErrors int     `json:"provenance_errors"`
}

// Segment records one checkpointed output segment.
type Segment struct {
	Index  int    `json:"index"`
	Frames int    `json:"frames"`
	Bytes  int64  `json:"bytes"`
	Offset int64  `json:"offset"` // byte offset of the segment inside frames.bin
	SHA256 string `json:"sha256"`
}

// Checkpoint is the resume state persisted after every segment.
type Checkpoint struct {
	Version        int               `json:"version"`
	InputSHA256    string            `json:"input_sha256"`
	ParamsSHA256   string            `json:"params_sha256"`
	Segments       []Segment         `json:"segments"`
	Complete       bool              `json:"complete"`
	Metrics        Metrics           `json:"metrics"`
	Outputs        map[string]string `json:"outputs"` // relpath -> sha256
	FullRunSeconds float64           `json:"full_run_seconds"`
}

// CheckpointVersion is the on-disk schema version.
const CheckpointVersion = 1

// SaveCheckpoint writes the checkpoint atomically: temp file, fsync,
// rename, so a crash can never leave a half-written checkpoint.
func SaveCheckpoint(path string, c *Checkpoint) error {
	c.Version = CheckpointVersion
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// LoadCheckpoint reads a checkpoint. A missing file is not an error
// (nil, nil); a corrupt file is a located, recoverable fault so the
// caller can fall back to the last valid boundary.
func LoadCheckpoint(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fault.Wrap(fault.KindResource, fault.Unknown, fault.Unknown,
			0, true, err, "checkpoint unreadable")
	}
	var c Checkpoint
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fault.Wrap(fault.KindCorrupt, fault.Unknown, fault.Unknown,
			0, true, err, "checkpoint corrupt; falling back to full run")
	}
	if c.Version != CheckpointVersion {
		return nil, fault.New(fault.KindCorrupt, fault.Unknown, fault.Unknown,
			0, true, "checkpoint version mismatch; falling back to full run")
	}
	return &c, nil
}

// writeFileAtomic writes data to path via a temp file in the same
// directory followed by fsync and rename.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// CheckSafePath rejects paths containing ".." segments (traversal) and
// empty paths. Absolute paths inside the workspace are allowed.
func CheckSafePath(p string) error {
	if p == "" {
		return fault.New(fault.KindResource, fault.Unknown, fault.Unknown,
			fault.Unknown, false, "empty path")
	}
	clean := filepath.Clean(p)
	for _, seg := range strings.Split(clean, string(filepath.Separator)) {
		if seg == ".." {
			return fault.New(fault.KindResource, fault.Unknown, fault.Unknown,
				fault.Unknown, false, fmt.Sprintf("path traversal rejected: %q", p))
		}
	}
	return nil
}
