package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/model"
)

const CheckpointVersion = 1

type SampleBoundary struct {
	OutputTrackID uint32 `json:"output_track_id"`
	Committed     int    `json:"committed"`
}

type Checkpoint struct {
	Version          int              `json:"version"`
	InputSHA256      string           `json:"input_sha256"`
	MappingDigest    string           `json:"mapping_digest"`
	OutputPath       string           `json:"output_path"`
	TotalSamples     int              `json:"total_samples"`
	TotalMediaBytes  int64            `json:"total_media_bytes"`
	CommittedSamples int              `json:"committed_samples"`
	Boundaries       []SampleBoundary `json:"boundaries"`
	PrefixBytes      int64            `json:"prefix_bytes"`
	PrefixSHA256     string           `json:"prefix_sha256"`
	Complete         bool             `json:"complete"`
	OutputSHA256     string           `json:"output_sha256,omitempty"`
}

func SaveCheckpoint(path string, c *Checkpoint) error {
	if c.Version == 0 {
		c.Version = CheckpointVersion
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return model.Wrap(model.CodeInternal, model.StageCheckpoint, "encode checkpoint", err)
	}
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return model.Wrap(model.CodeInternal, model.StageCheckpoint, "create checkpoint directory", err).WithSource(path)
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return model.Wrap(model.CodeInternal, model.StageCheckpoint, "write checkpoint", err).WithSource(path)
	}
	if err := os.Rename(tmp, path); err != nil {
		return model.Wrap(model.CodeInternal, model.StageCheckpoint, "commit checkpoint", err).WithSource(path)
	}
	return nil
}

func LoadCheckpoint(path string) (*Checkpoint, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, model.Wrap(model.CodeCheckpointCorrupt, model.StageCheckpoint, "read checkpoint", err).WithSource(path)
	}
	var c Checkpoint
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, model.Wrap(model.CodeCheckpointCorrupt, model.StageCheckpoint, "checkpoint JSON is corrupt", err).WithSource(path)
	}
	if c.Version != CheckpointVersion {
		return nil, model.Newf(model.CodeVersionMismatch, model.StageCheckpoint,
			"checkpoint version %d != %d", c.Version, CheckpointVersion).WithSource(path)
	}
	if len(c.PrefixSHA256) != 64 || (c.Complete && len(c.OutputSHA256) != 64) {
		return nil, model.New(model.CodeCheckpointCorrupt, model.StageCheckpoint, "checkpoint digest is missing or malformed").WithSource(path)
	}
	return &c, nil
}

func (c *Checkpoint) BindingsMatch(inputSHA, mappingDigest string) bool {
	return c.InputSHA256 == inputSHA && c.MappingDigest == mappingDigest
}

// VerifyAndTruncatePrefix hashes the committed prefix, removes any bytes
// after the last committed boundary, seeks to the append point, and returns a
// hasher already containing the verified prefix.
func (c *Checkpoint) VerifyAndTruncatePrefix(f *os.File) (hash.Hash, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, model.Wrap(model.CodeCheckpointCorrupt, model.StageCheckpoint, "stat output", err)
	}
	if info.Size() < c.PrefixBytes {
		return nil, model.Newf(model.CodeIntegrity, model.StageCheckpoint,
			"output size %d is shorter than checkpoint prefix %d", info.Size(), c.PrefixBytes)
	}
	h := sha256.New()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, model.Wrap(model.CodeInternal, model.StageCheckpoint, "seek prefix", err)
	}
	if _, err := io.CopyN(h, f, c.PrefixBytes); err != nil {
		return nil, model.Wrap(model.CodeIntegrity, model.StageCheckpoint, "hash committed prefix", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != c.PrefixSHA256 {
		return nil, model.New(model.CodeIntegrity, model.StageCheckpoint, "output prefix checksum mismatch").
			WithDigest(c.PrefixSHA256, got)
	}
	if err := f.Truncate(c.PrefixBytes); err != nil {
		return nil, model.Wrap(model.CodeInternal, model.StageCheckpoint, "truncate uncommitted tail", err)
	}
	if _, err := f.Seek(c.PrefixBytes, io.SeekStart); err != nil {
		return nil, model.Wrap(model.CodeInternal, model.StageCheckpoint, "seek append point", err)
	}
	return h, nil
}

func HashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", model.Wrap(model.CodeInvalidArgument, model.StagePipeline, "open for hash", err).WithSource(path)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", model.Wrap(model.CodeInternal, model.StagePipeline, "hash file", err).WithSource(path)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
