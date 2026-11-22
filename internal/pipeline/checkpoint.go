package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediarun/internal/mediaerr"
)

// Checkpoint binds a resumable run to exactly one (input, mapping) pair. On
// restart every binding is re-verified before a single sample is reprocessed:
// a changed input or mapping invalidates the checkpoint rather than silently
// continuing against different material.
type Checkpoint struct {
	Version       int    `json:"version"`
	InputSHA256   string `json:"input_sha256"`
	MappingSHA256 string `json:"mapping_sha256"`
	// LastCompleteSample is the count of samples durably present in the
	// output. Resume starts at this boundary, so a retried sample is never
	// counted twice.
	LastCompleteSample int64 `json:"last_complete_sample"`
	// OutputSHA256 is the hash of the partial output at the boundary. A
	// mismatch means the staged output was tampered with or lost.
	OutputSHA256 string `json:"output_sha256"`
}

const checkpointVersion = 1

// checkpointName is the single well-known file inside the work directory.
const checkpointName = "checkpoint.json"

// HashBytes returns the hex SHA-256 of raw content (used for inputs/configs).
func HashBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// HashFile streams a file's SHA-256 without loading it into memory.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(path)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 64*1024)); err != nil {
		return "", mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(path)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashConfig canonicalizes the mapping JSON then hashes it, so equivalent
// configurations hash identically regardless of map ordering.
func HashConfig(rawJSON []byte) (string, error) {
	var canonical any
	if err := json.Unmarshal(rawJSON, &canonical); err != nil {
		return "", mediaerr.New(mediaerr.CodeInvalidConfig,
			"mapping config is not valid JSON: "+err.Error())
	}
	norm, err := json.Marshal(canonical)
	if err != nil {
		return "", mediaerr.New(mediaerr.CodeInvalidConfig, err.Error())
	}
	return HashBytes(norm), nil
}

// SaveCheckpoint writes the checkpoint to a temp file and atomically renames
// it, so a crash can never leave a half-written checkpoint behind.
func SaveCheckpoint(workDir string, cp Checkpoint) error {
	cp.Version = checkpointVersion
	raw, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return mediaerr.New(mediaerr.CodeIO, err.Error())
	}
	final := filepath.Join(workDir, checkpointName)
	tmp, err := os.CreateTemp(workDir, ".checkpoint.tmp-*")
	if err != nil {
		return mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(final)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(final)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(final)
	}
	if err := tmp.Close(); err != nil {
		return mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(final)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(final)
	}
	return nil
}

// LoadCheckpoint reads and validates a checkpoint. A missing file returns
// (nil, nil) so a fresh run can begin; a malformed file is a hard error.
func LoadCheckpoint(workDir string) (*Checkpoint, error) {
	path := filepath.Join(workDir, checkpointName)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(path)
	}
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, mediaerr.New(mediaerr.CodeCheckpointMismatch,
			"unparseable checkpoint: "+err.Error()).OnPath(path)
	}
	if cp.Version != checkpointVersion {
		return nil, mediaerr.New(mediaerr.CodeCheckpointMismatch,
			fmt.Sprintf("checkpoint version %d unsupported", cp.Version)).OnPath(path)
	}
	return &cp, nil
}

// Verify checks every binding against the current run. Any mismatch is a
// CHECKPOINT_MISMATCH (exit 3), never a silent reuse of stale progress.
func (cp *Checkpoint) Verify(inputHash, mappingHash string) error {
	if cp.InputSHA256 != inputHash {
		return mediaerr.New(mediaerr.CodeCheckpointMismatch, fmt.Sprintf(
			"input changed: checkpoint=%s current=%s", cp.InputSHA256, inputHash))
	}
	if cp.MappingSHA256 != mappingHash {
		return mediaerr.New(mediaerr.CodeCheckpointMismatch, fmt.Sprintf(
			"mapping changed: checkpoint=%s current=%s", cp.MappingSHA256, mappingHash))
	}
	return nil
}

