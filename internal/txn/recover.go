package txn

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

// RecoverReport summarizes what recovery found and repaired.
type RecoverReport struct {
	ReplayedStages   []string `json:"replayed_stages"`
	DiscardedTemp    []string `json:"discarded_temps"`
	VerifiedStages   []string `json:"verified_stages"`
	BrokenStages     []string `json:"broken_stages"`
	LastChecksum     string   `json:"last_checksum"`
	RollbackChecksum string   `json:"rollback_checksum"`
}

// Recover opens a workspace after an interruption and reconciles it with the
// WAL. Only stages with a durable commit record survive; every prepared temp
// file and every stage whose content checksum no longer matches its commit is
// removed. The returned manager is ready to resume from the last good stage.
func Recover(root string) (*Manager, *RecoverReport, error) {
	m, err := NewManager(root)
	if err != nil {
		return nil, nil, err
	}
	rep := &RecoverReport{}
	committed, prepared, err := m.replayWAL(rep)
	if err != nil {
		return m, rep, err
	}
	// Every file in tmp/ is uncommitted by definition (a committed artifact
	// was renamed into stages/), so sweep the directory in addition to the
	// prepare records: this catches orphans whose prepare line was torn.
	entries, err := os.ReadDir(filepath.Join(m.root, tmpDir))
	if err != nil && !os.IsNotExist(err) {
		return m, rep, err
	}
	for _, e := range entries {
		prepared = append(prepared, filepath.Join(m.root, tmpDir, e.Name()))
	}
	// Prepared-but-uncommitted temp files are crash leftovers: delete them.
	for _, tmp := range prepared {
		if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
			return m, rep, err
		}
		rep.DiscardedTemp = append(rep.DiscardedTemp, filepath.Base(tmp))
	}
	// Sweep renamed-but-uncommitted orphans: any file under stages/ without a
	// durable commit record is a crash between the rename and the WAL commit.
	committedPaths := map[string]bool{}
	for _, rec := range committed {
		committedPaths[rec.RelPath] = true
	}
	stagesRoot := filepath.Join(m.root, stagesDir)
	if err := filepath.WalkDir(stagesRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(m.root, path)
		if rerr != nil {
			return rerr
		}
		if !committedPaths[rel] {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			rep.DiscardedTemp = append(rep.DiscardedTemp, filepath.Base(path))
		}
		return nil
	}); err != nil {
		return m, rep, err
	}
	// Verify every committed artifact against its recorded checksum.
	for stage, rec := range committed {
		full := filepath.Join(m.root, rec.RelPath)
		ok, err := verifyFile(full, rec.SHA256)
		if err != nil || !ok {
			rep.BrokenStages = append(rep.BrokenStages, stage)
			_ = os.Remove(full)
			delete(m.man.Stages, stage)
			continue
		}
		rep.VerifiedStages = append(rep.VerifiedStages, stage)
		rep.ReplayedStages = append(rep.ReplayedStages, stage)
	}
	m.reconcileManifest(rep)
	if err := m.writeManifest(); err != nil {
		return m, rep, err
	}
	return m, rep, nil
}

// replayWAL scans the log line by line. A trailing partial line (process
// killed mid-write) is ignored: the corresponding commit was never durable.
func (m *Manager) replayWAL(rep *RecoverReport) (map[string]walRecord, []string, error) {
	committed := map[string]walRecord{}
	var prepared []string
	prepareBySeq := map[uint64]string{}
	f, err := os.Open(filepath.Join(m.root, walName))
	if os.IsNotExist(err) {
		return committed, prepared, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), MaxStagePayload+1024)
	var maxSeq uint64
	for scanner.Scan() {
		var rec walRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue // partial/corrupt tail line: never a durable record
		}
		if rec.Seq > maxSeq {
			maxSeq = rec.Seq
		}
		switch rec.Op {
		case "prepare":
			prepared = append(prepared, rec.TmpPath)
			prepareBySeq[rec.Seq] = rec.TmpPath
		case "commit":
			committed[rec.Stage] = rec
			// A commit supersedes the prepare sharing its sequence number.
			if tmp, ok := prepareBySeq[rec.Seq]; ok {
				prepared = removePath(prepared, tmp)
			}
		case "rollback":
			delete(committed, rec.Stage)
		}
	}
	m.seq = maxSeq
	return committed, prepared, scanner.Err()
}

// reconcileManifest drops stages that failed verification and points the
// manifest at the most recent surviving committed stage.
func (m *Manager) reconcileManifest(rep *RecoverReport) {
	for stage := range m.man.Stages {
		if _, ok := m.man.Stages[stage]; ok && contains(rep.BrokenStages, stage) {
			delete(m.man.Stages, stage)
		}
	}
	if len(m.man.Stages) == 0 {
		m.man.LastStage = ""
		m.man.LastChecksum = m.man.RollbackChecksum
		return
	}
	var best StageInfo
	var bestStage string
	for stage, info := range m.man.Stages {
		if info.Seq >= best.Seq {
			best, bestStage = info, stage
		}
	}
	m.man.LastStage = bestStage
	m.man.LastChecksum = best.SHA256
	rep.LastChecksum = best.SHA256
	rep.RollbackChecksum = m.man.RollbackChecksum
}

// Rollback discards the output of a failed stage while preserving the last
// successfully committed checksum, exactly as recovery would after a crash.
func (m *Manager) Rollback(stage string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, ok := m.man.Stages[stage]
	if !ok {
		return m.man.LastChecksum, nil
	}
	if err := m.appendWAL(walRecord{Op: "rollback", Stage: stage, Seq: info.Seq,
		RelPath: info.RelPath, SHA256: info.SHA256}); err != nil {
		return "", err
	}
	_ = os.Remove(filepath.Join(m.root, info.RelPath))
	delete(m.man.Stages, stage)
	// The preserved checksum is the one committed immediately before the
	// rolled-back stage, never the failed stage's own checksum.
	kept := m.man.RollbackChecksum
	m.man.LastStage = ""
	m.man.LastChecksum = kept
	if err := m.writeManifest(); err != nil {
		return "", err
	}
	return kept, nil
}

// Committed reports whether a stage has a surviving committed artifact.
func (m *Manager) Committed(stage string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.man.Stages[stage]
	return ok
}

func verifyFile(path, want string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == want, nil
}

func removePath(paths []string, target string) []string {
	out := paths[:0]
	for _, p := range paths {
		if p != target {
			out = append(out, p)
		}
	}
	return out
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
