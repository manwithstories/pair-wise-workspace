package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"pairflow/internal/ingest"
	"pairflow/internal/model"
)

// Checkpoint is committed only after the corresponding output is durable.
type Checkpoint struct {
	AlgorithmVersion string `json:"algorithm_version"`
	Stage            string `json:"stage"`
	CompletedRecords int    `json:"completed_records"`
	OutputPath       string `json:"output_path"`
	OutputBytes      int64  `json:"output_bytes"`
	OutputSHA256     string `json:"output_sha256"`
	InputSHA256      string `json:"input_sha256"`
	ColdEqualToken   string `json:"cold_equal_token,omitempty"`
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// SaveCheckpoint atomically writes checkpoint.json and a stage backup.
func SaveCheckpoint(dir string, cp Checkpoint) (string, error) {
	if cp.AlgorithmVersion == "" {
		cp.AlgorithmVersion = model.AlgorithmVersion
	}
	if err := os.MkdirAll(filepath.Join(dir, "checkpoints"), 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return "", err
	}
	main := filepath.Join(dir, "checkpoint.json")
	if err := atomicWrite(main, b); err != nil {
		return "", err
	}
	stage := filepath.Join(dir, "checkpoints", cp.Stage+"-checkpoint.json")
	if err := atomicWrite(stage, b); err != nil {
		return "", err
	}
	return main, nil
}

// LoadCheckpoint reads and validates the main checkpoint.
func LoadCheckpoint(dir string) (*Checkpoint, *model.MediaError) {
	b, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, &model.MediaError{Code: model.CodeCheckpointCorrupt, Message: err.Error()}
	}
	var cp Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, &model.MediaError{Code: model.CodeCheckpointCorrupt, Message: err.Error()}
	}
	if cp.OutputPath != "" && cp.OutputSHA256 != "" {
		if merr := verifyCheckpointOutput(&cp); merr != nil {
			return nil, merr
		}
	}
	return &cp, nil
}

func verifyCheckpointOutput(cp *Checkpoint) *model.MediaError {
	out, err := os.ReadFile(cp.OutputPath)
	if err != nil {
		return &model.MediaError{Code: model.CodeCheckpointCorrupt, Message: "checkpoint output unavailable: " + err.Error()}
	}
	length := int64(len(out))
	if cp.OutputBytes > 0 && cp.OutputBytes <= length {
		length = cp.OutputBytes
	}
	if got := ingest.Sum256(out[:length]); got != cp.OutputSHA256 {
		return &model.MediaError{Code: model.CodeCheckpointCorrupt, ExpectedDigest: cp.OutputSHA256, ActualDigest: got, Message: "checkpoint output prefix digest mismatch"}
	}
	return nil
}

// RecoverCheckpoint returns the newest valid stage checkpoint if the main
// checkpoint is missing or corrupt. Corrupt files are left on disk for audit.
func RecoverCheckpoint(dir string, stages []string) (*Checkpoint, *model.MediaError) {
	cp, merr := LoadCheckpoint(dir)
	if merr == nil && cp != nil {
		return cp, nil
	}
	for i := len(stages) - 1; i >= 0; i-- {
		path := filepath.Join(dir, "checkpoints", stages[i]+"-checkpoint.json")
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var candidate Checkpoint
		if json.Unmarshal(b, &candidate) != nil {
			continue
		}
		if candidate.OutputPath != "" && candidate.OutputSHA256 != "" {
			if merr := verifyCheckpointOutput(&candidate); merr != nil {
				continue
			}
		}
		// Repair main checkpoint from the valid nearest stage.
		_, _ = SaveCheckpoint(dir, candidate)
		return &candidate, nil
	}
	if merr != nil {
		return nil, merr
	}
	return nil, nil
}
