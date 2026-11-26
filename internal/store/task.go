// Package store implements content-addressed task keys and atomic, resumable
// JSON checkpoints.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
)

// AnalyzerVersion is bumped when analysis semantics change. It participates in
// every task key and therefore invalidates old checkpoints.
const AnalyzerVersion = "mediascan-1.0.0"

// SchemaVersion identifies the on-disk checkpoint format.
const SchemaVersion = 1

var safeKey = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Params are the analysis parameters that can affect results. Workers is
// included to preserve the requested execution fingerprint even though it does
// not alter deterministic output.
type Params struct {
	Workers int   `json:"workers"`
	Seed    int64 `json:"seed"`
}

// Normalize clamps parameters to the supported range.
func (p Params) Normalize() Params {
	if p.Workers < 1 {
		p.Workers = 1
	}
	if p.Workers > 16 {
		p.Workers = 16
	}
	return p
}

// CanonicalParams returns deterministic JSON and its SHA-256 fingerprint.
func CanonicalParams(p Params) ([]byte, string, error) {
	p = p.Normalize()
	b, err := json.Marshal(p)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

// Task is the idempotent identity of analyzing one asset with one analyzer
// version and parameter set.
type Task struct {
	Key               string `json:"task_key"`
	JobID             string `json:"job_id"`
	AssetName         string `json:"asset_name"`
	AssetSHA256       string `json:"asset_sha256"`
	AnalyzerVersion   string `json:"analyzer_version"`
	Params            Params `json:"params"`
	ParamsFingerprint string `json:"params_fingerprint"`
}

type taskIdentity struct {
	AssetSHA256       string `json:"asset_sha256"`
	AnalyzerVersion   string `json:"analyzer_version"`
	ParamsFingerprint string `json:"params_fingerprint"`
}

// NewTask computes the deterministic task key. The key covers asset bytes,
// analyzer version, and canonical parameters.
func NewTask(jobID, assetName, assetSHA256 string, p Params) (Task, error) {
	p = p.Normalize()
	_, fp, err := CanonicalParams(p)
	if err != nil {
		return Task{}, err
	}
	t := Task{
		JobID:             jobID,
		AssetName:         filepath.Base(assetName),
		AssetSHA256:       assetSHA256,
		AnalyzerVersion:   AnalyzerVersion,
		Params:            p,
		ParamsFingerprint: fp,
	}
	id := taskIdentity{
		AssetSHA256:       t.AssetSHA256,
		AnalyzerVersion:   t.AnalyzerVersion,
		ParamsFingerprint: fp,
	}
	b, err := json.Marshal(id)
	if err != nil {
		return Task{}, err
	}
	sum := sha256.Sum256(b)
	t.Key = hex.EncodeToString(sum[:])
	return t, nil
}

// JobID returns a safe fixed-length logical ID for a local absolute path.
func JobID(absolutePath string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(absolutePath)))
	return hex.EncodeToString(sum[:])
}

// ValidKey reports whether k is a bare SHA-256 hex key and therefore cannot
// escape the checkpoint directory.
func ValidKey(k string) bool { return safeKey.MatchString(k) }

// MismatchError explains why an old checkpoint cannot be resumed.
type MismatchError struct {
	Reasons []string
	OldKey  string
	NewKey  string
}

func (e *MismatchError) Error() string {
	if e == nil || len(e.Reasons) == 0 {
		return "checkpoint fingerprint mismatch"
	}
	msg := "checkpoint fingerprint mismatch:"
	for _, r := range e.Reasons {
		msg += "\n  - " + r
	}
	return msg
}

// IsMismatch reports whether err is a checkpoint fingerprint rejection.
func IsMismatch(err error) bool {
	var m *MismatchError
	return AsMismatch(err, &m)
}

// AsMismatch is a small errors.As helper without importing errors in callers.
func AsMismatch(err error, target **MismatchError) bool {
	for err != nil {
		if m, ok := err.(*MismatchError); ok {
			*target = m
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Verify compares a checkpoint task with the current task.
func Verify(cp *Checkpoint, want Task) error {
	if cp == nil {
		return &MismatchError{Reasons: []string{"checkpoint is empty"}, NewKey: want.Key}
	}
	var reasons []string
	old := cp.Task
	if old.AssetSHA256 != want.AssetSHA256 {
		reasons = append(reasons, fmt.Sprintf("asset SHA-256 changed: checkpoint=%s current=%s", old.AssetSHA256, want.AssetSHA256))
	}
	if old.AnalyzerVersion != want.AnalyzerVersion {
		reasons = append(reasons, fmt.Sprintf("analyzer version changed: checkpoint=%s current=%s", old.AnalyzerVersion, want.AnalyzerVersion))
	}
	if old.ParamsFingerprint != want.ParamsFingerprint {
		reasons = append(reasons, fmt.Sprintf("parameters changed: checkpoint=%s current=%s", old.ParamsFingerprint, want.ParamsFingerprint))
	}
	if old.Key != want.Key {
		reasons = append(reasons, fmt.Sprintf("task key changed: checkpoint=%s current=%s", old.Key, want.Key))
	}
	if len(reasons) > 0 {
		return &MismatchError{Reasons: reasons, OldKey: old.Key, NewKey: want.Key}
	}
	return nil
}
