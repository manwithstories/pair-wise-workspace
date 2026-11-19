// Package batch persists per-block checkpoints (temp file + atomic rename)
// and restores interrupted batches. On resume it validates the input
// fingerprint, the config digest and every completed block checksum;
// anything that does not match is recomputed from the last safe block.
package batch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"review/internal/safepath"
)

const checkpointName = "checkpoint.json"

// BlockState records one finished block.
type BlockState struct {
	Cam    int    `json:"cam"`
	Index  int    `json:"index"`
	Frames int    `json:"frames"`
	SHA256 string `json:"sha256"`
}

// Checkpoint is the resumable state of one batch.
type Checkpoint struct {
	Version          int          `json:"version"`
	InputFingerprint string       `json:"input_fingerprint"`
	ConfigDigest     string       `json:"config_digest"`
	TotalBlocks      int          `json:"total_blocks"`
	Done             []BlockState `json:"done"`
}

// Load reads the checkpoint, or returns nil when none exists. A corrupt
// checkpoint is an error (exit code 3 territory), not a silent restart.
func Load(work string) (*Checkpoint, error) {
	p, err := safepath.Join(work, checkpointName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Checkpoint
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("checkpoint corrupt: %w", err)
	}
	return &c, nil
}

// Save writes the checkpoint atomically: temp file, fsync, rename.
func (c *Checkpoint) Save(work string) error {
	p, err := safepath.Join(work, checkpointName)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Add records a finished block, keeping Done sorted by (cam, index) so the
// checkpoint file is deterministic.
func (c *Checkpoint) Add(b BlockState) {
	c.Done = append(c.Done, b)
	sort.Slice(c.Done, func(i, j int) bool {
		if c.Done[i].Cam != c.Done[j].Cam {
			return c.Done[i].Cam < c.Done[j].Cam
		}
		return c.Done[i].Index < c.Done[j].Index
	})
}

// Has reports whether a block is already done.
func (c *Checkpoint) Has(cam, index int) bool {
	for _, b := range c.Done {
		if b.Cam == cam && b.Index == index {
			return true
		}
	}
	return false
}

// FingerprintFiles hashes the sorted input files into one fingerprint.
func FingerprintFiles(paths []string) (string, error) {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, p := range sorted {
		f, err := os.Open(p)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
		fmt.Fprintf(h, "|%s\n", p)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DigestConfig hashes the canonical JSON of the run configuration.
func DigestConfig(v interface{}) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
