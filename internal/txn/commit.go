// Package txn commits pipeline stages atomically.  Each stage writes its
// artifact to a temporary file, journals a prepare record with the artifact
// digest, renames the temporary file into place (an atomic boundary), then
// journals the commit.  Recovery replays the journal: a prepared-but-
// uncommitted stage is rolled back to its previous artifact, and a committed
// stage is adopted without recomputation.  Only complete, verified stages are
// ever made visible.
package txn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"archivecheck/internal/fault"
	"archivecheck/internal/journal"
)

// Stage names the ordered pipeline steps.
type Stage string

const (
	StageParse    Stage = "parse"
	StageRepair   Stage = "repair"
	StageIndex    Stage = "index"
	StageManifest Stage = "manifest"
)

// Order is the pipeline's execution order.
var Order = []Stage{StageParse, StageRepair, StageIndex, StageManifest}

// Artifact is a stage's produced file plus its identity.
type Artifact struct {
	Stage  Stage
	Path   string
	Digest string
	Bytes  int64
	Meta   json.RawMessage
}

// Validator re-checks a stage's artifact before commit; nil means accept.
type Validator func(a Artifact) error

// Manager coordinates staged, journaled commits for one pipeline run.
type Manager struct {
	dir    string
	out    string
	wal    *journal.WAL
	faults *fault.Set

	mu        sync.Mutex
	rollbacks int
	committed map[Stage]Artifact
}

// Config configures the manager.
type Config struct {
	// Dir holds the journal; OutDir receives committed artifacts.
	Dir    string
	OutDir string
	Faults *fault.Set
}

// New opens the journal under cfg.Dir and reconstructs committed state.
// It returns the manager and the set of stages already committed by a prior
// run, so the caller can resume without redoing them.
func New(cfg Config) (*Manager, map[Stage]Artifact, error) {
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, nil, err
	}
	walPath := filepath.Join(cfg.Dir, "pipeline.wal")
	w, recs, err := journal.Open(walPath)
	if err != nil {
		return nil, nil, err
	}
	m := &Manager{dir: cfg.Dir, out: cfg.OutDir, wal: w, faults: cfg.Faults, committed: make(map[Stage]Artifact)}
	if w.Truncated > 0 && m.faults != nil {
		m.faults.Add(fault.New(fault.ModuleJournal, fault.KindJournalTail, fault.NoOffset(),
			fmt.Sprintf("discarded %d torn byte(s) from journal tail", w.Truncated)))
	}
	done, err := m.recover(recs)
	if err != nil {
		w.Close()
		return nil, nil, err
	}
	return m, done, nil
}

// recover replays the journal.  A stage committed in a prior run is adopted
// if its artifact still exists and matches the recorded digest; otherwise it
// is rolled back so it will be recomputed.
func (m *Manager) recover(recs []journal.Record) (map[Stage]Artifact, error) {
	type stageState struct {
		prepared  Artifact
		committed *Artifact
		lastOp    journal.Op
	}
	states := make(map[Stage]*stageState)
	for _, r := range recs {
		st := r.Stage
		s := states[Stage(st)]
		if s == nil {
			s = &stageState{}
			states[Stage(st)] = s
		}
		s.lastOp = r.Op
		switch r.Op {
		case journal.OpPrepare:
			var a Artifact
			if len(r.Payload) > 0 {
				_ = json.Unmarshal(r.Payload, &a)
			}
			a.Stage = Stage(st)
			a.Digest = r.Digest
			s.prepared = a
		case journal.OpCommit:
			a := s.prepared
			a.Stage = Stage(st)
			if r.Digest != "" {
				a.Digest = r.Digest
			}
			cp := a
			s.committed = &cp
		}
	}
	done := make(map[Stage]Artifact)
	names := make([]Stage, 0, len(states))
	for n := range states {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	for _, n := range names {
		s := states[n]
		switch {
		case s.committed != nil:
			a := *s.committed
			ok, err := artifactIntact(a)
			if err != nil {
				return nil, err
			}
			if ok {
				m.committed[a.Stage] = a
				done[a.Stage] = a
				continue
			}
			// Committed but the artifact vanished or was corrupted: roll it
			// back so the stage is recomputed.
			m.rollback(n, "committed artifact missing or digest mismatch")
			if m.faults != nil {
				m.faults.Add(fault.New(fault.ModuleTxn, fault.KindVerification, fault.NoOffset(),
					fmt.Sprintf("stage %q was committed but its artifact no longer verifies; marked for redo", n)))
			}
		case s.lastOp == journal.OpPrepare || s.lastOp == journal.OpBegin:
			// Prepared but never committed: remove any temp artifact.
			m.rollback(n, "prepared without commit")
		}
	}
	return done, nil
}

func artifactIntact(a Artifact) (bool, error) {
	if a.Path == "" {
		return false, nil
	}
	f, err := os.Open(a.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return a.Digest == "" || hex.EncodeToString(h.Sum(nil)) == a.Digest, nil
}

// Committed returns the adopted committed artifacts.
func (m *Manager) Committed() map[Stage]Artifact {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[Stage]Artifact, len(m.committed))
	for k, v := range m.committed {
		out[k] = v
	}
	return out
}

// Rollbacks returns how many stages were rolled back.
func (m *Manager) Rollbacks() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rollbacks
}

// Stage runs fn to produce a temporary artifact, validates it, and commits it
// atomically.  A validation error or a failed write rolls the stage back and
// returns the error; the journal never records a commit for it.
func (m *Manager) Stage(stage Stage, meta json.RawMessage, validate Validator, fn func(tmp string) error) (Artifact, error) {
	m.mu.Lock()
	if prev, ok := m.committed[stage]; ok {
		m.mu.Unlock()
		return prev, nil
	}
	m.mu.Unlock()

	if _, err := m.wal.Append(journal.Record{Stage: string(stage), Op: journal.OpBegin}); err != nil {
		return Artifact{}, err
	}
	tmp, err := os.CreateTemp(filepath.Join(m.out, ".tmp"), string(stage)+"-*.part")
	if err != nil {
		if m.dir != "" {
			_ = os.MkdirAll(filepath.Join(m.out, ".tmp"), 0o755)
			if tmp, err = os.CreateTemp(filepath.Join(m.out, ".tmp"), string(stage)+"-*.part"); err != nil {
				return Artifact{}, err
			}
		} else {
			return Artifact{}, err
		}
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}
	if err := fn(tmpPath); err != nil {
		cleanup()
		m.rollback(stage, "producer failed: "+err.Error())
		return Artifact{}, err
	}
	digest, size, err := digestFile(tmpPath)
	if err != nil {
		cleanup()
		m.rollback(stage, "digest failed")
		return Artifact{}, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		m.rollback(stage, "sync failed")
		return Artifact{}, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		m.rollback(stage, "close failed")
		return Artifact{}, err
	}
	final := filepath.Join(m.out, string(stage)+".artifact")
	a := Artifact{Stage: stage, Path: final, Digest: digest, Bytes: size, Meta: meta}
	if validate != nil {
		if err := validate(a); err != nil {
			os.Remove(tmpPath)
			m.rollback(stage, "validation failed: "+err.Error())
			return Artifact{}, err
		}
	}
	prep, err := json.Marshal(Artifact{Stage: stage, Path: final, Bytes: size, Meta: meta})
	if err != nil {
		os.Remove(tmpPath)
		return Artifact{}, err
	}
	if _, err := m.wal.Append(journal.Record{Stage: string(stage), Op: journal.OpPrepare, Payload: prep, Digest: digest}); err != nil {
		os.Remove(tmpPath)
		return Artifact{}, err
	}
	// Atomic boundary: rename within the same directory replaces the old
	// artifact in one step.
	if err := os.Rename(tmpPath, final); err != nil {
		os.Remove(tmpPath)
		m.rollback(stage, "rename failed")
		return Artifact{}, err
	}
	if err := syncDir(m.out); err != nil {
		m.rollback(stage, "directory sync failed")
		return Artifact{}, err
	}
	if _, err := m.wal.Append(journal.Record{Stage: string(stage), Op: journal.OpCommit, Digest: digest}); err != nil {
		// The artifact is visible but uncommitted; recovery will roll it back.
		return Artifact{}, err
	}
	m.mu.Lock()
	m.committed[stage] = a
	m.mu.Unlock()
	return a, nil
}

func (m *Manager) rollback(stage Stage, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollbacks++
	delete(m.committed, stage)
	if reason != "" && m.faults != nil {
		m.faults.Add(fault.New(fault.ModuleTxn, fault.KindInterrupted, fault.NoOffset(),
			fmt.Sprintf("rolled back stage %q: %s", stage, reason)))
	}
}

func digestFile(path string) (string, int64, error) {
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

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Close closes the journal.
func (m *Manager) Close() error { return m.wal.Close() }

// WALPath exposes the journal path for fault injection in tests and demos.
func (m *Manager) WALPath() string { return m.wal.Path() }
