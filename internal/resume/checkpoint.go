// Package resume stores safe-cut checkpoints and verifies prefix/output hashes.
package resume

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/mediaerr"
	"mediad/internal/verify"
)

const CheckpointVersion = 1

// InputState binds a checkpoint to an exact input prefix.
type InputState struct {
	ID              string `json:"id"`
	Path            string `json:"path"`
	Size            int64  `json:"size"`
	PrefixOffset    int64  `json:"prefix_offset"`
	PrefixSHA256    string `json:"prefix_sha256"`
	SafeOffset      int64  `json:"safe_offset"`
	PacketsOccurred int    `json:"packets_occurred"`
	LastSafePTS     int64  `json:"last_safe_pts"`
}

// OutputState is one durably committed artifact.
type OutputState struct {
	Key    string `json:"key"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int    `json:"size"`
}

// Checkpoint is the resumable pipeline state.
type Checkpoint struct {
	Version        int                    `json:"version"`
	Seed           int64                  `json:"seed"`
	Dir            string                 `json:"dir"`
	Inputs         []InputState           `json:"inputs"`
	Outputs        map[string]OutputState `json:"outputs"`
	MasterSHA256   string                 `json:"master_sha256,omitempty"`
	Completed      bool                   `json:"completed"`
	InterruptAfter int                    `json:"interrupt_after,omitempty"`
}

// New creates an initialized checkpoint.
func New(seed int64, dir string, interruptAfter int) *Checkpoint {
	return &Checkpoint{
		Version:        CheckpointVersion,
		Seed:           seed,
		Dir:            dir,
		Outputs:        map[string]OutputState{},
		InterruptAfter: interruptAfter,
	}
}

// Load reads a checkpoint. A missing checkpoint returns nil and no error.
func Load(path string) (*Checkpoint, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, mediaerr.Wrap(mediaerr.KindInvalidArgument, -1, 0, true, err)
	}
	var c Checkpoint
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, mediaerr.Wrap(mediaerr.KindChecksumMismatch, -1, 0, false, err)
	}
	if c.Version != CheckpointVersion {
		return nil, mediaerr.New(mediaerr.KindUnknownVersion, -1, 0, false,
			"checkpoint version %d unsupported", c.Version)
	}
	if c.Outputs == nil {
		c.Outputs = map[string]OutputState{}
	}
	return &c, nil
}

// Save atomically writes the checkpoint.
func (c *Checkpoint) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return mediaerr.Wrap(mediaerr.KindInvalidArgument, -1, 0, false, err)
	}
	b = append(b, '\n')
	return verify.AtomicWriteFile(path, b, 0o600)
}

// HashPrefix returns SHA-256 of the first n bytes of path.
func HashPrefix(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", mediaerr.Wrap(mediaerr.KindPrefixMismatch, -1, 0, false, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, f, n); err != nil {
		return "", mediaerr.Wrap(mediaerr.KindPrefixMismatch, -1, 0, false, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RecordInputPrefix calculates and stores an input prefix hash.
func (c *Checkpoint) RecordInputPrefix(id, path string, prefixOffset, safeOffset int64, packets int, safePTS int64) error {
	st, err := os.Stat(path)
	if err != nil {
		return mediaerr.Wrap(mediaerr.KindPrefixMismatch, -1, 0, false, err)
	}
	if prefixOffset > st.Size() {
		return mediaerr.New(mediaerr.KindPrefixMismatch, prefixOffset, 0, false,
			"prefix offset exceeds input size %d", st.Size())
	}
	hash, err := HashPrefix(path, prefixOffset)
	if err != nil {
		return err
	}
	for i := range c.Inputs {
		if c.Inputs[i].ID == id {
			c.Inputs[i] = InputState{ID: id, Path: path, Size: st.Size(), PrefixOffset: prefixOffset,
				PrefixSHA256: hash, SafeOffset: safeOffset, PacketsOccurred: packets, LastSafePTS: safePTS}
			return nil
		}
	}
	c.Inputs = append(c.Inputs, InputState{ID: id, Path: path, Size: st.Size(), PrefixOffset: prefixOffset,
		PrefixSHA256: hash, SafeOffset: safeOffset, PacketsOccurred: packets, LastSafePTS: safePTS})
	return nil
}

// VerifyPrefixes ensures every checkpointed input prefix is unchanged.
func (c *Checkpoint) VerifyPrefixes() error {
	for _, in := range c.Inputs {
		got, err := HashPrefix(in.Path, in.PrefixOffset)
		if err != nil {
			return err
		}
		if got != in.PrefixSHA256 {
			return mediaerr.New(mediaerr.KindPrefixMismatch, in.PrefixOffset, 0, false,
				"input %q prefix sha256=%s want %s", in.ID, got, in.PrefixSHA256)
		}
	}
	return nil
}

// IsCommitted reports whether key already has the exact same output hash.
func (c *Checkpoint) IsCommitted(key, sha string) bool {
	o, ok := c.Outputs[key]
	return ok && o.SHA256 == sha
}

// CommitOutput atomically writes an artifact unless the identical artifact is
// already committed. It records the hash only after rename succeeds.
func (c *Checkpoint) CommitOutput(baseDir, key, relPath string, data []byte) (replayed bool, err error) {
	sum := verify.SHA256Bytes(data)
	if existing, ok := c.Outputs[key]; ok {
		if existing.SHA256 != sum {
			return false, mediaerr.New(mediaerr.KindOutputMismatch, -1, 0, false,
				"output key %s hash changed despite existing checkpoint", key)
		}
		full := relPath
		if !filepath.IsAbs(full) {
			full = filepath.Join(baseDir, relPath)
		}
		if err := verify.VerifyFile(full, sum); err == nil {
			return true, nil
		}
		// Hash record is valid but file is missing/corrupt; rewrite atomically.
	}
	full := relPath
	if !filepath.IsAbs(full) {
		full = filepath.Join(baseDir, relPath)
	}
	if err := verify.AtomicWriteFile(full, data, 0o644); err != nil {
		return false, err
	}
	c.Outputs[key] = OutputState{Key: key, Path: relPath, SHA256: sum, Size: len(data)}
	return false, nil
}

// VerifyOutputs checks every checkpointed output hash under baseDir.
func (c *Checkpoint) VerifyOutputs(baseDir string) error {
	for _, o := range c.Outputs {
		p := o.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(baseDir, o.Path)
		}
		if err := verify.VerifyFile(p, o.SHA256); err != nil {
			return err
		}
	}
	return nil
}
