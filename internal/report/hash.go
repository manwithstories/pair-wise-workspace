// Package report owns durable segment storage, the resume manifest and the
// independent verification pass that re-hashes files on disk. It also builds
// the JSON summary the CLI prints. Nothing here trusts in-memory hashes:
// resume and verification always read the bytes back from storage.
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"archivetl/internal/clock"
	"archivetl/internal/pipeline"
)

// ManifestVersion is bumped when the on-disk layout changes incompatibly.
const ManifestVersion = 1

// ManifestEntry records one committed segment and its provenance.
type ManifestEntry struct {
	ID            int     `json:"id"`
	InputHash     string  `json:"input_hash"`
	OutputHash    string  `json:"output_hash"`
	Bytes         int64   `json:"bytes"`
	StartTick     int64   `json:"start_tick"`
	EndTick       int64   `json:"end_tick"`
	Events        int     `json:"events"`
	KeyHits       int     `json:"keyframe_hits"`
	Skipped       bool    `json:"skipped"`
	FailedOffsets []int64 `json:"failed_offsets,omitempty"`
}

// Manifest is the full resume state, marshalled deterministically.
type Manifest struct {
	Version int                      `json:"version"`
	Entries map[string]ManifestEntry `json:"entries"`
}

// FileStore implements pipeline.SegmentStore against a directory.
type FileStore struct {
	dir      string
	manifest Manifest
}

// NewFileStore opens (or creates) the output directory and loads its
// manifest when one exists.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("output directory is empty")
	}
	if err := os.MkdirAll(filepath.Join(dir, "segments"), 0o755); err != nil {
		return nil, err
	}
	s := &FileStore{dir: dir, manifest: Manifest{
		Version: ManifestVersion, Entries: map[string]ManifestEntry{},
	}}
	if err := s.loadManifest(); err != nil {
		return nil, err
	}
	return s, nil
}

func entryKey(id int) string { return fmt.Sprintf("seg-%05d", id) }

func (s *FileStore) segmentPath(id int) string {
	return filepath.Join(s.dir, "segments", entryKey(id)+".afr")
}

func (s *FileStore) manifestPath() string {
	return filepath.Join(s.dir, "manifest.json")
}

func (s *FileStore) loadManifest() error {
	raw, err := os.ReadFile(s.manifestPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &s.manifest); err != nil {
		return fmt.Errorf("manifest unreadable: %w", err)
	}
	if s.manifest.Version != ManifestVersion {
		return fmt.Errorf("manifest version %d unsupported, want %d",
			s.manifest.Version, ManifestVersion)
	}
	if s.manifest.Entries == nil {
		s.manifest.Entries = map[string]ManifestEntry{}
	}
	return nil
}

// HashFile streams a file's SHA-256 without loading it fully into memory.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func hashBytes(p []byte) string {
	sum := sha256.Sum256(p)
	return hex.EncodeToString(sum[:])
}

// HasMatchingOutput independently verifies a previous output: the input
// fingerprint must match and the file on disk must still hash to the
// recorded output hash. Any divergence forces recomputation.
func (s *FileStore) HasMatchingOutput(id int, inputHash string) (bool, error) {
	entry, ok := s.manifest.Entries[entryKey(id)]
	if !ok || inputHash == "" || entry.InputHash != inputHash {
		return false, nil
	}
	got, _, err := HashFile(s.segmentPath(id))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return got == entry.OutputHash, nil
}

// WriteSegment persists payload atomically (temp file, sync, rename) and
// then rewrites the manifest atomically.
func (s *FileStore) WriteSegment(seg *pipeline.Segment, payload []byte) error {
	path := s.segmentPath(seg.ID)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(payload); err != nil {
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
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	actual := hashBytes(payload)
	if seg.OutputHash != "" && actual != seg.OutputHash {
		return fmt.Errorf("segment %d output hash mismatch before commit", seg.ID)
	}
	entry := ManifestEntry{
		ID: seg.ID, InputHash: seg.InputHash, OutputHash: actual,
		Bytes:     int64(len(payload)),
		StartTick: int64(seg.Start), EndTick: int64(seg.End),
		Events: seg.Summary.Accepted, KeyHits: seg.KeyHits,
		FailedOffsets: append([]int64(nil), seg.FailedOffsets...),
	}
	s.manifest.Entries[entryKey(seg.ID)] = entry
	return s.persistManifest()
}

func (s *FileStore) persistManifest() error {
	raw, err := json.MarshalIndent(s.manifest, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.manifestPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.manifestPath())
}

// MarkSkipped records that a segment was reused without recomputation.
func (s *FileStore) MarkSkipped(id int) error {
	key := entryKey(id)
	entry, ok := s.manifest.Entries[key]
	if !ok {
		return fmt.Errorf("cannot skip unknown segment %d", id)
	}
	entry.Skipped = true
	s.manifest.Entries[key] = entry
	return s.persistManifest()
}

// Manifest exposes a copy of the current manifest state.
func (s *FileStore) Manifest() Manifest {
	out := Manifest{Version: s.manifest.Version, Entries: map[string]ManifestEntry{}}
	for k, v := range s.manifest.Entries {
		out.Entries[k] = v
	}
	return out
}

// Verify re-hashes every committed file and returns the ids whose on-disk
// bytes no longer match the manifest.
func (s *FileStore) Verify() ([]int, error) {
	var mismatched []int
	for key, entry := range s.manifest.Entries {
		got, n, err := HashFile(s.segmentPath(entry.ID))
		if err != nil {
			return nil, fmt.Errorf("verify %s: %w", key, err)
		}
		if got != entry.OutputHash || n != entry.Bytes {
			mismatched = append(mismatched, entry.ID)
		}
	}
	return mismatched, nil
}

var _ = clock.TickRate // keep the time-base dependency documented
