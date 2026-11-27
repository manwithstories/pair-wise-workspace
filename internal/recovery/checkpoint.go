// Package recovery persists per-node pipeline state so an interrupted run
// can resume without redoing work whose inputs did not change. A node is
// reusable only when its input digest, configuration digest and the parser
// version all match the current run.
package recovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"fastpost/internal/metadata"
)

// checkpointFile is the single on-disk state file inside the run directory.
const checkpointFile = "checkpoint.json"

// NodeState captures everything needed to decide whether one pipeline
// node's cached result is still valid.
type NodeState struct {
	NodeID       string `json:"node_id"`
	InputDigest  string `json:"input_digest"`
	ConfigDigest string `json:"config_digest"`
	ParserVer    string `json:"parser_ver"`
	// OutputDigest covers the artifacts this node produced, used to verify
	// integrity on resume rather than trust the cache blindly.
	OutputDigest string `json:"output_digest"`
	Done         bool   `json:"done"`
}

// Checkpoint is the serializable resume state for a whole run.
type Checkpoint struct {
	RunID string      `json:"run_id"`
	Nodes []NodeState `json:"nodes"`
}

// Store loads and saves checkpoints and answers reuse questions.
type Store struct {
	path string
}

// NewStore binds a store to the run state directory.
func NewStore(dir string) *Store {
	return &Store{path: filepath.Join(dir, checkpointFile)}
}

// Load reads an existing checkpoint. A missing file returns (nil, nil),
// which the caller treats as a fresh run.
func (s *Store) Load() (*Checkpoint, error) {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, fmt.Errorf("corrupt checkpoint: %w", err)
	}
	return &cp, nil
}

// Save atomically writes the checkpoint via a temp file and rename.
func (s *Store) Save(cp *Checkpoint) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Clear removes a checkpoint, forcing the next run to start fresh.
func (s *Store) Clear() error {
	err := os.Remove(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Reusable reports whether a node's cached state matches the current
// inputs, configuration and parser version. Any mismatch invalidates it.
func Reusable(state NodeState, inputDigest, configDigest string) bool {
	return state.Done &&
		state.InputDigest == inputDigest &&
		state.ConfigDigest == configDigest &&
		state.ParserVer == metadata.ParserVersion
}

// Plan is the resume decision for one run. Reusable nodes keep their
// cached output; every node in Redo must execute again, including all
// downstream dependents of any node whose input changed.
type Plan struct {
	Reusable []string
	Redo     []string
}

// BuildPlan compares desired per-node digests against a loaded checkpoint
// and the supplied downstream closure. changedInputs lists node IDs whose
// input digest differs; their full downstream set is added to Redo.
func BuildPlan(
	cp *Checkpoint,
	desired map[string]NodeState,
	topoOrder []string,
	downstream func(nodeID string) []string,
) Plan {
	cached := map[string]NodeState{}
	if cp != nil {
		for _, n := range cp.Nodes {
			cached[n.NodeID] = n
		}
	}
	redo := map[string]bool{}
	for id, want := range desired {
		have, ok := cached[id]
		if !ok || !Reusable(have, want.InputDigest, want.ConfigDigest) {
			redo[id] = true
			// Invalidate every transitive dependent as well.
			for _, dep := range downstream(id) {
				redo[dep] = true
			}
		}
	}
	plan := Plan{}
	for _, id := range topoOrder {
		if redo[id] {
			plan.Redo = append(plan.Redo, id)
		} else {
			plan.Reusable = append(plan.Reusable, id)
		}
	}
	return plan
}

// Digest renders a stable lowercase hex SHA-256 over the concatenation of
// the supplied parts. Callers pass raw input bytes and canonicalized
// configuration so equivalent inputs hash identically.
func Digest(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
