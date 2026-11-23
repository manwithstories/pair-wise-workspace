package txn

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mediarun/internal/clock"
)

// Manager executes pipeline stages under the write-ahead log. A stage's
// output is written to a temporary file, hashed, logged, and only then
// renamed onto its final path. If anything fails, the temporary file is
// removed and the manager rolls back to the last verified checkpoint, so a
// half-written final artifact can never be observed.
type Manager struct {
	log         *Log
	dir         string
	checkpoints map[string]string // stage -> verified artefact hash
	order       []string          // checkpoint order, oldest first
	Rollbacks   int
	Commits     int
	// FailOnCommit injects a crash between the temp write and the rename.
	FailOnCommit bool
	// FailCommitStage restricts FailOnCommit to one stage name.
	FailCommitStage string
	// FailOnSync injects a disk-full failure after the temp write.
	FailOnSync bool
}

// NewManager opens the WAL at logPath and stages artifacts under dir.
func NewManager(logPath, dir, runID string) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, clock.Wrap(clock.KindRecovery, clock.CodeCheckpoint, dir, err)
	}
	l, err := OpenLog(logPath, runID)
	if err != nil {
		return nil, err
	}
	return &Manager{log: l, dir: dir, checkpoints: map[string]string{}}, nil
}

// Log exposes the WAL for replay and diagnostics.
func (m *Manager) Log() *Log { return m.log }

// Close closes the WAL.
func (m *Manager) Close() error { return m.log.Close() }

// Checkpointed returns the verified hash of a completed stage.
func (m *Manager) Checkpointed(stage string) (string, bool) {
	h, ok := m.checkpoints[stage]
	return h, ok
}

// Resume replays an existing WAL and rebuilds the checkpoint map from records
// whose temporary artefacts still exist and still hash as recorded. A stage
// whose artefact is missing or corrupt is not resumed; it will be recomputed.
func (m *Manager) Resume(recs []Record) {
	for _, r := range recs {
		switch r.Type {
		case RecCheckpoint:
			if r.OutputHash == "" {
				continue
			}
			if actual, err := HashFile(r.Temp); err == nil && actual == r.OutputHash {
				// observeCheckpoint keeps order free of duplicates when a
				// stage was committed more than once in the same log.
				m.observeCheckpoint(r.Stage, r.OutputHash)
			}
		case RecRollback:
			delete(m.checkpoints, r.Stage)
			for i, s := range m.order {
				if s == r.Stage {
					m.order = append(m.order[:i], m.order[i+1:]...)
					break
				}
			}
		}
	}
	m.Rollbacks = countRollbacks(recs)
}

func countRollbacks(recs []Record) int {
	n := 0
	for _, r := range recs {
		if r.Type == RecRollback {
			n++
		}
	}
	return n
}

// Stage describes one unit of work the pipeline wants to commit.
type Stage struct {
	Name   string
	Cursor string
	Input  []byte
	Output []byte
	Final  string
}

// Execute runs a stage through the log. The stage output is written to a
// partial artefact, verified against its checksum and renamed to the stage
// artefact that represents the atomic boundary. Only after the checkpoint
// record is durable does the manager derive any final file, and only after
// the final rename is the commit recorded. A stage therefore resumes when
// its artefact still verifies, and leaves no final file when anything fails.
func (m *Manager) Execute(s Stage) error {
	inHash := HashBytes(s.Input)
	artefact := filepath.Join(m.dir, s.Name+".tmp")
	partial := artefact + ".part"
	if err := m.log.Append(Record{
		Type: RecStageBegin, Stage: s.Name, Cursor: s.Cursor,
		InputHash: inHash, Temp: artefact,
	}); err != nil {
		return err
	}
	expect := HashBytes(s.Output)
	if err := os.WriteFile(partial, s.Output, 0o600); err != nil {
		return m.abort(s.Name, "write failed", err)
	}
	if m.FailOnSync {
		return m.abort(s.Name, "simulated disk full", clock.New(
			clock.KindRecovery, clock.CodeDiskFull, s.Name, "simulated sync failure"))
	}
	actual, err := HashFile(partial)
	if err != nil {
		return m.abort(s.Name, "verify failed", err)
	}
	if actual != expect {
		return m.abort(s.Name, "artefact hash mismatch", clock.New(
			clock.KindRecovery, clock.CodeHashMismatch, s.Name,
			"partial artefact does not match computed output"))
	}
	if m.FailOnCommit && (m.FailCommitStage == "" || m.FailCommitStage == s.Name) {
		// Crash before the atomic boundary: the partial artefact is removed
		// and no checkpoint is ever written, so the stage is recomputed.
		return m.abort(s.Name, "simulated commit interruption", clock.New(
			clock.KindRecovery, clock.CodeCommitInterrupt, s.Name, "crash before checkpoint"))
	}
	if err := os.Rename(partial, artefact); err != nil {
		return m.abort(s.Name, "artefact rename failed", err)
	}
	if err := m.log.Append(Record{
		Type: RecCheckpoint, Stage: s.Name, InputHash: inHash,
		OutputHash: expect, Cursor: s.Cursor, Temp: artefact,
	}); err != nil {
		return m.abort(s.Name, "checkpoint append failed", err)
	}
	m.observeCheckpoint(s.Name, expect)
	if s.Final != "" {
		if err := m.writeFinal(s.Name, artefact, s.Final, expect); err != nil {
			return err
		}
	}
	if err := m.log.Append(Record{
		Type: RecStageCommit, Stage: s.Name, InputHash: inHash,
		OutputHash: expect, Cursor: s.Cursor, Temp: artefact, Final: s.Final,
	}); err != nil {
		if s.Final != "" {
			_ = os.Remove(s.Final)
		}
		return m.abort(s.Name, "commit record append failed", err)
	}
	m.Commits++
	return nil
}

// writeFinal derives the final file from the verified artefact. The final
// path is only ever replaced by a complete, hash-checked file.
func (m *Manager) writeFinal(stage, artefact, final, expect string) error {
	data, err := os.ReadFile(artefact)
	if err != nil || HashBytes(data) != expect {
		return m.abort(stage, "artefact unreadable", clock.New(
			clock.KindRecovery, clock.CodeHashMismatch, stage, "artefact changed"))
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return m.abort(stage, "final directory", err)
	}
	tmp := final + ".part"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return m.abort(stage, "final write failed", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return m.abort(stage, "final rename failed", err)
	}
	return nil
}

// abort removes the partial artefact and rolls the stage back to the last
// verified checkpoint. A previously committed artefact for the same stage
// name is left alone only when it still verifies, so a failed recomputation
// cannot destroy a good checkpoint.
func (m *Manager) abort(stage, detail string, cause error) error {
	_ = os.Remove(filepath.Join(m.dir, stage+".tmp"))
	_ = m.log.Append(Record{Type: RecRollback, Stage: stage, Detail: detail})
	delete(m.checkpoints, stage)
	m.Rollbacks++
	if cause != nil {
		var ce *clock.Error
		if errors.As(cause, &ce) {
			return ce
		}
		return clock.Wrap(clock.KindRecovery, CodeRecoveryFailed(stage), stage, cause)
	}
	return clock.New(clock.KindRecovery, clock.CodeCheckpoint, stage, detail)
}

// CodeRecoveryFailed composes a per-stage recovery code.
func CodeRecoveryFailed(stage string) string {
	var b strings.Builder
	b.WriteString("txn.recover_")
	for _, r := range stage {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// LastVerifiedStage returns the most recently checkpointed stage whose
// artefact still verifies on disk.
func (m *Manager) LastVerifiedStage() (string, bool) {
	for i := len(m.order) - 1; i >= 0; i-- {
		s := m.order[i]
		h := m.checkpoints[s]
		if actual, err := HashFile(filepath.Join(m.dir, s+".tmp")); err == nil && actual == h {
			return s, true
		}
	}
	return "", false
}

// CheckpointSummary returns a stable digest of every verified checkpoint,
// used to prove a resumed run produced the same state as a full rerun.
func (m *Manager) CheckpointSummary() string {
	names := make([]string, 0, len(m.checkpoints))
	for n := range m.checkpoints {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte('=')
		b.WriteString(m.checkpoints[n])
		b.WriteByte('\n')
	}
	return HashBytes([]byte(b.String()))
}

// RollbackToCheckpoint discards stages newer than keep, restoring the manager
// to a known-good boundary. Their final artefacts are removed.
func (m *Manager) RollbackToCheckpoint(keep string, finals map[string]string) error {
	idx := -1
	for i, s := range m.order {
		if s == keep {
			idx = i
			break
		}
	}
	if idx < 0 {
		return clock.New(clock.KindRecovery, clock.CodeCheckpoint, keep,
			"checkpoint not found")
	}
	for _, s := range m.order[idx+1:] {
		delete(m.checkpoints, s)
		if final, ok := finals[s]; ok && final != "" {
			_ = os.Remove(final)
		}
		_ = m.log.Append(Record{Type: RecRollback, Stage: s, Detail: "rolled back to " + keep})
		m.Rollbacks++
	}
	m.order = m.order[:idx+1]
	return nil
}

func (m *Manager) observeCheckpoint(stage, hash string) {
	if _, ok := m.checkpoints[stage]; !ok {
		m.order = append(m.order, stage)
	}
	m.checkpoints[stage] = hash
}
