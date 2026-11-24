package txn

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediarun/internal/model"
	"mediarun/internal/verify"
)

// Manifest is the durable list of committed segments. It is replaced
// atomically: the new file is written to a temporary name inside the same
// directory, fsynced and then renamed over the old one.
type Manifest struct {
	Version  int               `json:"version"`
	Segments []SegmentCommit   `json:"segments"`
	Stages   map[string]string `json:"stages"`
}

// SegmentCommit is one atomic slice of the delivery.
type SegmentCommit struct {
	Name   string `json:"name"`
	Start  int64  `json:"start"`
	End    int64  `json:"end"`
	Events int    `json:"events"`
	SHA256 string `json:"sha256"`
	Gaps   int    `json:"gaps"`
}

// CurrentManifestVersion guards against stale or foreign manifests.
const CurrentManifestVersion = 1

// copyStages copies a stage map so callers cannot mutate a manifest alias.
func copyStages(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func manifestPath(dir string) string { return filepath.Join(dir, "manifest.json") }

// LoadManifest reads and validates the manifest. A missing manifest yields
// an empty, valid manifest so a first run can proceed.
func LoadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(manifestPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return &Manifest{Version: CurrentManifestVersion, Stages: map[string]string{}}, nil
		}
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: manifest: %v", model.ErrCorruptIndex, err)
	}
	if m.Version != CurrentManifestVersion {
		return nil, fmt.Errorf("%w: manifest version %d", model.ErrCorruptIndex, m.Version)
	}
	if m.Stages == nil {
		m.Stages = map[string]string{}
	}
	return &m, nil
}

// Validate checks that the manifest is internally consistent: names unique,
// hashes well-formed, ranges ordered.
func (m *Manifest) Validate() error {
	seen := map[string]bool{}
	lastEnd := int64(-1)
	for i, s := range m.Segments {
		if s.Name == "" {
			return fmt.Errorf("%w: segment %d has no name", model.ErrCorruptIndex, i)
		}
		if seen[s.Name] {
			return fmt.Errorf("%w: duplicate segment %q", model.ErrCorruptIndex, s.Name)
		}
		seen[s.Name] = true
		if len(s.SHA256) != 64 {
			return fmt.Errorf("%w: segment %q hash %q", model.ErrCorruptIndex, s.Name, s.SHA256)
		}
		if _, err := hex.DecodeString(s.SHA256); err != nil {
			return fmt.Errorf("%w: segment %q hash: %v", model.ErrCorruptIndex, s.Name, err)
		}
		if s.Events < 0 || s.Gaps < 0 || s.End < s.Start {
			return fmt.Errorf("%w: segment %q has invalid counts", model.ErrCorruptIndex, s.Name)
		}
		if s.Start < lastEnd {
			return fmt.Errorf("%w: segment %q overlaps its predecessor", model.ErrCorruptIndex, s.Name)
		}
		lastEnd = s.End
	}
	return nil
}

// SaveManifest atomically replaces the manifest with a copy of m.
func SaveManifest(dir string, m *Manifest) error {
	if m.Version == 0 {
		m.Version = CurrentManifestVersion
	}
	if m.Stages == nil {
		m.Stages = map[string]string{}
	}
	if err := m.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(name)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, manifestPath(dir)); err != nil {
		os.Remove(name)
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// SegmentFile is the canonical name of a segment's payload on disk.
func SegmentFile(name string) string { return "seg-" + name + ".json" }

// WriteSegment writes the segment payload and returns its canonical hash.
// The file is not considered committed until the manifest names it.
func WriteSegment(dir, name string, payload []byte) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: segment name", model.ErrInvalid)
	}
	file := filepath.Join(dir, SegmentFile(name))
	tmp, err := os.CreateTemp(dir, ".seg-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, file); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return verify.BytesHash(payload), nil
}
