// Package checkpoint persists and verifies atomic relay processing state.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pairwise/relay/internal/media"
)

// Schema is the current checkpoint format version.
const Schema = "relay-checkpoint-v1"

// StageState records completion and a deterministic digest for one stage.
type StageState struct {
	Name      string `json:"name"`
	Completed bool   `json:"completed"`
	Digest    string `json:"digest,omitempty"`
}

// Artifact records one output file and its content hash.
type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Checkpoint is the atomically replaced recovery record.
type Checkpoint struct {
	Schema            string       `json:"schema"`
	Seed              int64        `json:"seed"`
	Rungs             int          `json:"rungs"`
	Segments          int          `json:"segments"`
	InputSHA256       string       `json:"input_sha256"`
	ProcessedSegments int          `json:"processed_segments"`
	InterruptedAfter  int          `json:"interrupt_after,omitempty"`
	Stages            []StageState `json:"stages"`
	Artifacts         []Artifact   `json:"artifacts,omitempty"`
	OutputSHA256      string       `json:"output_sha256,omitempty"`
	Completed         bool         `json:"completed"`
}

// Save writes JSON via a same-directory temp file and atomic rename.
func Save(path string, cp *Checkpoint) error {
	if cp == nil {
		return media.NewFault(media.CodeCheckpoint, media.FragmentID{}, -1, false, "nil checkpoint")
	}
	cp.Schema = Schema
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return media.NewExitError(4, err)
	}
	raw, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return media.NewExitError(4, err)
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return media.NewExitError(4, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return media.NewExitError(4, err)
	}
	if err := f.Close(); err != nil {
		return media.NewExitError(4, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return media.NewExitError(4, err)
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// Load reads and parses a checkpoint.
func Load(path string) (*Checkpoint, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, media.NewExitError(4, media.NewFault(media.CodeCheckpoint, media.FragmentID{}, -1, true, "checkpoint not found: %s", path))
	}
	if err != nil {
		return nil, media.NewExitError(4, err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, media.NewExitError(4, media.NewFault(media.CodeCheckpoint, media.FragmentID{}, -1, false, "invalid checkpoint JSON: %v", err))
	}
	if cp.Schema != Schema {
		return nil, media.NewExitError(4, media.NewFault(media.CodeCheckpoint, media.FragmentID{}, -1, false, "unsupported checkpoint schema %q", cp.Schema))
	}
	return &cp, nil
}

// ValidateInput ensures the checkpoint belongs to the exact generated input.
func (cp *Checkpoint) ValidateInput(inputSHA string) error {
	if cp.InputSHA256 == "" || cp.InputSHA256 != inputSHA {
		return media.NewExitError(4, media.NewFault(media.CodeCheckpoint, media.FragmentID{}, -1, false,
			"input digest mismatch: checkpoint=%s current=%s", cp.InputSHA256, inputSHA))
	}
	if cp.Segments <= 0 || cp.Rungs <= 0 {
		return media.NewExitError(4, media.NewFault(media.CodeCheckpoint, media.FragmentID{}, -1, false, "checkpoint dimensions are invalid"))
	}
	return nil
}

// Stage returns the named stage state.
func (cp *Checkpoint) Stage(name string) (StageState, bool) {
	for _, s := range cp.Stages {
		if s.Name == name {
			return s, true
		}
	}
	return StageState{}, false
}

// SetStage upserts a stage state.
func (cp *Checkpoint) SetStage(name, digest string, done bool) {
	for i := range cp.Stages {
		if cp.Stages[i].Name == name {
			cp.Stages[i].Digest = digest
			cp.Stages[i].Completed = done
			return
		}
	}
	cp.Stages = append(cp.Stages, StageState{Name: name, Digest: digest, Completed: done})
}

// VerifyArtifacts recomputes every recorded artifact hash under root and then
// verifies the aggregate output digest.
func (cp *Checkpoint) VerifyArtifacts(root string) ([]Artifact, error) {
	if len(cp.Artifacts) == 0 {
		return nil, media.NewExitError(4, media.NewFault(media.CodeArtifactMismatch, media.FragmentID{}, -1, false, "checkpoint has no artifacts"))
	}
	checked := make([]Artifact, 0, len(cp.Artifacts))
	for _, a := range cp.Artifacts {
		if err := safeRelative(a.Path); err != nil {
			return nil, media.NewExitError(4, err)
		}
		full := filepath.Join(root, a.Path)
		sum, size, err := HashFile(full)
		if err != nil {
			return nil, media.NewExitError(4, media.NewFault(media.CodeArtifactMismatch, media.FragmentID{}, -1, true, "artifact missing %s: %v", a.Path, err))
		}
		if sum != a.SHA256 || size != a.Size {
			return nil, media.NewExitError(4, media.NewFault(media.CodeArtifactMismatch, media.FragmentID{}, -1, false,
				"artifact %s hash/size mismatch: recorded=%s/%d actual=%s/%d", a.Path, a.SHA256, a.Size, sum, size))
		}
		checked = append(checked, Artifact{Path: a.Path, SHA256: sum, Size: size})
	}
	agg, err := HashArtifactSet(checked)
	if err != nil {
		return nil, media.NewExitError(4, err)
	}
	if cp.OutputSHA256 != "" && agg != cp.OutputSHA256 {
		return nil, media.NewExitError(4, media.NewFault(media.CodeArtifactMismatch, media.FragmentID{}, -1, false,
			"aggregate output digest mismatch: checkpoint=%s recomputed=%s", cp.OutputSHA256, agg))
	}
	return checked, nil
}

// HashBytes returns the lowercase SHA-256 of data.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// HashFile hashes one file and returns its size.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// HashArtifactSet deterministically hashes sorted path/hash/size records.
func HashArtifactSet(arts []Artifact) (string, error) {
	cp := append([]Artifact(nil), arts...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Path < cp[j].Path })
	h := sha256.New()
	for _, a := range cp {
		if _, err := fmt.Fprintf(h, "%s\n%s\n%d\n", a.Path, a.SHA256, a.Size); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func safeRelative(p string) error {
	if p == "" || filepath.IsAbs(p) {
		return media.NewFault(media.CodeUnsafePath, media.FragmentID{}, -1, false, "artifact path must be relative: %q", p)
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return media.NewFault(media.CodeUnsafePath, media.FragmentID{}, -1, false, "artifact path escapes root: %q", p)
	}
	return nil
}
