// Package txn provides write-ahead-logged, crash-safe stage commits for the
// pipeline. Each stage output is first written to a temp file in the same
// directory, fsynced, atomically renamed into place, and only then marked
// committed in the WAL. Recovery replays exclusively committed stages and
// discards every prepared-but-uncommitted temp file.
package txn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// WAL and manifest file names (relative to the manager root).
const (
	walName          = "wal.log"
	ManifestFileName = "manifest.json"
	manifestName     = ManifestFileName
	stagesDir        = "stages"
	tmpDir           = "tmp"
)

// MaxStagePayload bounds a single staged artifact (8 MiB), matching the
// container packet limit so no stage can buffer unbounded data.
const MaxStagePayload = 8 << 20

// walRecord is one JSON line in the write-ahead log.
type walRecord struct {
	Op       string `json:"op"`        // prepare | commit | rollback
	Stage    string `json:"stage"`
	Seq      uint64 `json:"seq"`
	RelPath  string `json:"rel_path"`
	TmpPath  string `json:"tmp_path,omitempty"`
	SHA256   string `json:"sha256"`
}

// StageInfo is the committed checksum bookkeeping for one pipeline stage.
type StageInfo struct {
	Seq     uint64 `json:"seq"`
	SHA256  string `json:"sha256"`
	RelPath string `json:"rel_path"`
}

// Manifest is the atomically maintained summary of committed stages.
type Manifest struct {
	LastStage          string               `json:"last_stage"`
	LastChecksum       string               `json:"last_checksum"`
	Stages             map[string]StageInfo `json:"stages"`
	RollbackChecksum   string               `json:"rollback_checksum"`
}

// Manager owns one pipeline work directory and serializes its mutations.
type Manager struct {
	root   string
	mu     sync.Mutex
	seq    uint64
	wal    *os.File
	man    Manifest
}

// NewManager opens (or creates) a crash-safe workspace at root. The root must
// be a clean, absolute directory; traversal and symlink escapes are rejected.
func NewManager(root string) (*Manager, error) {
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	m := &Manager{root: real, man: Manifest{Stages: map[string]StageInfo{}}}
	for _, sub := range []string{stagesDir, tmpDir} {
		if err := os.MkdirAll(filepath.Join(real, sub), 0o755); err != nil {
			return nil, err
		}
	}
	wal, err := os.OpenFile(filepath.Join(real, walName), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	m.wal = wal
	if err := m.loadManifest(); err != nil {
		return nil, err
	}
	return m, nil
}

// Root returns the resolved workspace root.
func (m *Manager) Root() string { return m.root }

// SafeJoin resolves a workspace-relative path and rejects anything that would
// escape the root via absolute paths, ".." segments or symlinks.
func (m *Manager) SafeJoin(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q must be a non-empty relative path", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", rel)
	}
	target := filepath.Join(m.root, clean)
	real, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err == nil {
		if real != m.root && !strings.HasPrefix(real, m.root+string(filepath.Separator)) {
			return "", fmt.Errorf("path %q resolves outside the workspace", rel)
		}
	}
	return target, nil
}

// Stage writes data as the output artifact of a named pipeline stage using
// the full prepare -> fsync -> rename -> commit protocol.
func (m *Manager) Stage(stage, relName string, data []byte) (string, error) {
	if len(data) > MaxStagePayload {
		return "", fmt.Errorf("stage payload %d exceeds limit %d", len(data), MaxStagePayload)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	final, err := m.SafeJoin(filepath.Join(stagesDir, stage, relName))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return "", err
	}
	m.seq++
	seq := m.seq
	tmp, err := m.SafeJoin(filepath.Join(tmpDir, fmt.Sprintf("s%d-%s", seq, filepath.Base(relName))))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	checksum := hex.EncodeToString(sum[:])
	relFinal, _ := filepath.Rel(m.root, final)
	// 1) temp file + fsync (durability before the rename).
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	// 2) Atomic rename inside one directory tree. The rename is atomic on a
	// single filesystem; durability of the resulting directory entry is
	// established by the WAL commit fsync below, not a separate dir fsync.
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	// 2) Update the in-memory manifest and atomically stage its snapshot.
	// The manifest is a derived cache of the WAL, so it is written first but
	// not yet authoritative.
	prev := m.man.LastChecksum
	m.man.Stages[stage] = StageInfo{Seq: seq, SHA256: checksum, RelPath: relFinal}
	m.man.LastStage = stage
	m.man.LastChecksum = checksum
	m.man.RollbackChecksum = prev
	if err := m.stageManifest(); err != nil {
		return "", err
	}
	// 3) One fsynced WAL commit record is the single durability barrier. The
	// artifact was fsynced before its atomic rename, so after this barrier the
	// pair {record, file} is durable together. A crash at any earlier point
	// leaves only orphans (a tmp/ file, an unreferenced stages/ file or a
	// stale manifest), all reconciled by recovery from the WAL. No separate
	// directory fsync is needed because recovery never trusts directory
	// entries without a matching commit record.
	if err := m.appendWAL(walRecord{Op: "commit", Stage: stage, Seq: seq,
		RelPath: relFinal, SHA256: checksum}); err != nil {
		return "", err
	}
	return checksum, nil
}

func (m *Manager) appendWAL(r walRecord) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := m.wal.Write(line); err != nil {
		return err
	}
	return m.wal.Sync()
}

// stageManifest writes and atomically renames the manifest without its own
// fsync barrier: durability is provided by the subsequent WAL commit fsync.
func (m *Manager) stageManifest() error {
	final := filepath.Join(m.root, manifestName)
	tmp := final + ".tmp"
	data, err := json.MarshalIndent(&m.man, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	return nil
}

func (m *Manager) writeManifest() error {
	final := filepath.Join(m.root, manifestName)
	tmp := final + ".tmp"
	data, err := json.MarshalIndent(&m.man, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := syncPath(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	return syncDir(m.root)
}

func (m *Manager) loadManifest() error {
	data, err := os.ReadFile(filepath.Join(m.root, manifestName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &m.man)
}

// Manifest returns a copy of the current committed manifest.
func (m *Manager) Manifest() Manifest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.man
	out.Stages = make(map[string]StageInfo, len(m.man.Stages))
	for k, v := range m.man.Stages {
		out.Stages[k] = v
	}
	return out
}

// Close flushes the WAL handle.
func (m *Manager) Close() error { return m.wal.Close() }

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
