// Package resume persists a small run journal so an interrupted recovery can
// continue without recomputing. Reuse is refused when either the input set or
// the effective configuration changed, and the refusal is reported as a stable
// JSON record rather than an opaque error.
package resume

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

	"archive.local/recover/internal/errs"
)

// StateVersion guards the journal format.
const StateVersion = 1

// State is the persisted journal.
type State struct {
	Version      int                 `json:"version"`
	InputDigest  string              `json:"input_digest"`
	ConfigDigest string              `json:"config_digest"`
	Completed    []string            `json:"completed"`
	Artifacts    map[string]Artifact `json:"artifacts"`
}

// Artifact names one produced file and its digest so stale outputs can be
// detected without trusting mtime.
type Artifact struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Bytes  int64  `json:"bytes"`
}

// Decision is the outcome of a resume attempt.
type Decision struct {
	Reuse        bool      `json:"reuse"`
	InputDigest  string    `json:"input_digest"`
	ConfigDigest string    `json:"config_digest"`
	Reason       string    `json:"reason,omitempty"`
	Issues       errs.List `json:"issues"`
}

// DigestInputs hashes the ordered (name, contents) pairs of every input file.
// Ordering is normalized by name so a different directory walk order does not
// change the digest.
func DigestInputs(inputs map[string][]byte) string {
	names := make([]string, 0, len(inputs))
	for n := range inputs {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%d:%s\n", len(n), n)
		sum := sha256.Sum256(inputs[n])
		fmt.Fprintf(h, "%d:%s\n", len(inputs[n]), hex.EncodeToString(sum[:]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// DigestConfig hashes the effective configuration. Values are rendered through
// JSON so nested settings hash deterministically.
func DigestConfig(cfg any) (string, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("config digest: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Load reads a journal. A missing file is not an error; it means no prior run.
func Load(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("decode resume state: %w", err)
	}
	if s.Version != StateVersion {
		return nil, fmt.Errorf("resume state version %d unsupported", s.Version)
	}
	if s.Artifacts == nil {
		s.Artifacts = map[string]Artifact{}
	}
	return &s, nil
}

// Check decides whether the prior journal may be reused for these digests.
func Check(prev *State, inputDigest, configDigest string) *Decision {
	d := &Decision{InputDigest: inputDigest, ConfigDigest: configDigest}
	if prev == nil {
		d.Reason = "no previous run"
		return d
	}
	loc := errs.UnknownLocation()
	if prev.InputDigest != inputDigest {
		d.Reason = "input digest changed"
		d.Issues.AddFatal(errs.ClassDigest, errs.CodeDigestInput,
			fmt.Sprintf("input digest changed: have %s, state says %s", short(inputDigest), short(prev.InputDigest)), loc)
		return d
	}
	if prev.ConfigDigest != configDigest {
		d.Reason = "config digest changed"
		d.Issues.AddFatal(errs.ClassDigest, errs.CodeDigestConfig,
			fmt.Sprintf("config digest changed: have %s, state says %s", short(configDigest), short(prev.ConfigDigest)), loc)
		return d
	}
	// Artifacts must still exist and match their recorded digests; otherwise
	// the journal is optimistic and reuse would silently mix generations.
	for name, a := range prev.Artifacts {
		if a.Path == "" {
			continue
		}
		if _, err := os.Stat(a.Path); err != nil {
			d.Reason = fmt.Sprintf("artifact %q missing", name)
			d.Issues.AddFatal(errs.ClassDigest, errs.CodeDigestInput,
				fmt.Sprintf("recorded artifact %q is gone: %v", a.Path, err), loc)
			return d
		}
	}
	d.Reuse = true
	d.Reason = "digests and artifacts match"
	return d
}

// Save writes the journal atomically through a temp file and rename.
func Save(path string, s *State) error {
	if s.Version == 0 {
		s.Version = StateVersion
	}
	if s.Artifacts == nil {
		s.Artifacts = map[string]Artifact{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".resume-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// MarkCompleted records a finished stage and its artifact.
func (s *State) MarkCompleted(stage string, path string, b []byte) {
	if s.Artifacts == nil {
		s.Artifacts = map[string]Artifact{}
	}
	s.Completed = append(s.Completed, stage)
	if path != "" {
		sum := sha256.Sum256(b)
		s.Artifacts[stage] = Artifact{Path: path, Digest: hex.EncodeToString(sum[:]), Bytes: int64(len(b))}
	}
}

// IsCompleted reports whether a stage is already done.
func (s *State) IsCompleted(stage string) bool {
	for _, c := range s.Completed {
		if c == stage {
			return true
		}
	}
	return false
}

// ReadAll reads an input file with a hard size bound.
func ReadAll(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > limit {
		return nil, fmt.Errorf("%s: %d bytes exceeds limit %d", path, st.Size(), limit)
	}
	return io.ReadAll(io.LimitReader(f, limit+1))
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
