package index

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"archivecheck/internal/fault"
)

// CheckpointVersion is bumped whenever the on-disk checkpoint layout changes.
const CheckpointVersion = 3

// Checkpoint captures everything needed to prove resumability: input manifest
// digest, committed-output digest, and the index plus timeline state.
type Checkpoint struct {
	Version            int               `json:"version"`
	InputDigest        string            `json:"input_digest"`
	CommittedDigest    string            `json:"committed_digest"`
	Committed          int               `json:"committed"`
	CommittedFragments []string          `json:"committed_fragments"`
	LastCommitted      string            `json:"last_committed"`
	FragmentsTotal     int               `json:"fragments_total"`
	RepairedTfdt       int               `json:"repaired_tfdt"`
	MissingMfhd        int               `json:"missing_mfhd"`
	OutputBytes        int64             `json:"output_bytes"`
	NextDecodeTime     map[string]uint64 `json:"next_decode_time"`
	Index              *Index            `json:"index"`
	Checksum           string            `json:"checksum"`
}

func (c *Checkpoint) canonicalBytes() ([]byte, error) {
	clone := *c
	clone.Checksum = ""
	clone.Index = nil
	raw, err := json.Marshal(&clone)
	if err != nil {
		return nil, err
	}
	if c.Index == nil {
		return raw, nil
	}
	idxRaw, err := json.Marshal(c.Index)
	if err != nil {
		return nil, err
	}
	return append(raw, idxRaw...), nil
}

// Seal computes and stores the checkpoint checksum.
func (c *Checkpoint) Seal() error {
	raw, err := c.canonicalBytes()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	c.Checksum = hex.EncodeToString(sum[:])
	return nil
}

// Verify recomputes and compares the checksum.
func (c *Checkpoint) Verify() error {
	if c.Version != CheckpointVersion {
		return fault.New(fault.KindCheckpoint, "", "", 0,
			fmt.Sprintf("checkpoint version %d unsupported (want %d)", c.Version, CheckpointVersion))
	}
	if c.Checksum == "" {
		return fault.New(fault.KindCheckpoint, "", "", 0, "checkpoint has no checksum")
	}
	want := c.Checksum
	c.Checksum = ""
	err := c.Seal()
	c.Checksum = want
	if err != nil {
		return fault.Wrap(err, fault.KindCheckpoint, "", "", 0, "cannot digest checkpoint")
	}
	if c.Checksum != want {
		return fault.New(fault.KindCheckpoint, "", "", 0, "checkpoint checksum mismatch")
	}
	return nil
}

// Encode serialises a sealed checkpoint.
func Encode(c *Checkpoint) ([]byte, error) {
	if err := c.Seal(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(c, "", "  ")
}

// Decode parses and verifies checkpoint bytes.
func Decode(raw []byte) (*Checkpoint, error) {
	var c Checkpoint
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fault.Wrap(err, fault.KindCheckpoint, "", "", 0, "cannot decode checkpoint")
	}
	if err := c.Verify(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save writes a checkpoint via a temp file in the same directory, fsyncs it
// and atomically renames it into place, so a crash never leaves a partial
// checkpoint at the destination.
func Save(path string, c *Checkpoint) error {
	raw, err := Encode(c)
	if err != nil {
		return fault.Wrap(err, fault.KindCheckpoint, "", "", 0, "cannot encode checkpoint")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fault.Wrap(err, fault.KindCheckpoint, "", "", 0, "cannot create checkpoint directory")
	}
	tmp, err := os.CreateTemp(dir, ".checkpoint-*.tmp")
	if err != nil {
		return fault.Wrap(err, fault.KindCheckpoint, "", "", 0, "cannot create temp checkpoint")
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		cleanup()
		return fault.Wrap(err, fault.KindCheckpoint, "", "", 0, "cannot write temp checkpoint")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fault.Wrap(err, fault.KindCheckpoint, "", "", 0, "cannot fsync temp checkpoint")
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fault.Wrap(err, fault.KindCheckpoint, "", "", 0, "cannot close temp checkpoint")
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fault.Wrap(err, fault.KindCheckpoint, "", "", 0, "cannot atomically replace checkpoint")
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// Load reads a checkpoint and rejects a mismatched version or checksum.
func Load(path string) (*Checkpoint, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.Wrap(err, fault.KindCheckpoint, "", "", 0, "cannot read checkpoint")
	}
	return Decode(raw)
}
