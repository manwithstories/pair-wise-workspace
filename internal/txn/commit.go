// Package txn coordinates stage commits, crash recovery and rollback. Every
// stage writes a begin record, completes its output bytes, fsyncs, and only
// then appends a commit record holding the output length and prefix digest.
// Recovery accepts only complete commit boundaries; any later bytes are rolled
// back so repeated recovery is byte-for-byte idempotent.
package txn

import (
	"os"
	"path/filepath"
	"time"

	"mediamerge/internal/checksum"
	"mediamerge/internal/journal"
	"mediamerge/internal/mapping"
	"mediamerge/internal/model"
	"mediamerge/internal/mux"
)

// Manager owns one output transaction (one input, one output, one journal).
type Manager struct {
	Dir       string
	Output    string
	Journal   string
	InputSHA  string
	InputSize int64
	Plan      *mapping.Plan
	Layout    *mux.Layout
	Log       *journal.Log
}

// Paths lays out the fixed file names inside dir.
func Paths(dir string) (output, journalPath string) {
	return filepath.Join(dir, "output.mp4"), filepath.Join(dir, "stages.log")
}

// New prepares a fresh transaction, truncating any old journal and output.
func New(dir string, plan *mapping.Plan, inputSHA string, inputSize int64) (*Manager, error) {
	output, jpath := Paths(dir)
	m := &Manager{Dir: dir, Output: output, Journal: jpath, InputSHA: inputSHA,
		InputSize: inputSize, Plan: plan}
	lay := mux.PlanLayout(plan)
	m.Layout = lay
	return m, nil
}

// Begin writes a begin record for a stage.
func (m *Manager) Begin(stage int, committed int64) error {
	if err := m.ensureLog(); err != nil {
		return err
	}
	return m.Log.Append(journal.Record{
		Stage: stage, Kind: journal.KindBegin, InputSHA256: m.InputSHA,
		InputSize: m.InputSize, OutputLen: committed,
	})
}

// Commit fsyncs outputLen bytes of the output and then appends the matching
// commit record. The digest covers exactly the committed prefix.
func (m *Manager) Commit(stage int, f *os.File, outputLen int64) error {
	if err := f.Sync(); err != nil {
		return model.WrapError(model.StageJournal, model.CodeWrite, outputLen, 0, true, err,
			"sync stage %d output", stage)
	}
	return m.CommitSynced(stage, outputLen)
}

// CommitSynced appends the commit record for an already synced prefix.
func (m *Manager) CommitSynced(stage int, outputLen int64) error {
	prefix, err := checksum.Prefix(m.Output, outputLen)
	if err != nil {
		return model.WrapError(model.StageJournal, model.CodeIntegrity, outputLen, 0, false, err,
			"digest stage %d prefix", stage)
	}
	if err := m.ensureLog(); err != nil {
		return err
	}
	return m.Log.Append(journal.Record{
		Stage: stage, Kind: journal.KindCommit, InputSHA256: m.InputSHA,
		InputSize: m.InputSize, OutputLen: outputLen, PrefixSHA: prefix,
	})
}

// CommitFile commits the current on-disk prefix of length outputLen.
func (m *Manager) CommitFile(stage int, outputLen int64) error {
	f, err := os.OpenFile(m.Output, os.O_RDWR, 0o644)
	if err != nil {
		return model.WrapError(model.StageJournal, model.CodeWrite, outputLen, 0, false, err,
			"open output for commit")
	}
	defer f.Close()
	return m.Commit(stage, f, outputLen)
}

// Close flushes and closes the journal.
func (m *Manager) Close() error {
	if m.Log == nil {
		return nil
	}
	err := m.Log.Close()
	m.Log = nil
	return err
}

func (m *Manager) ensureLog() error {
	if m.Log != nil {
		return nil
	}
	if err := rejectSymlink(m.Output); err != nil {
		return err
	}
	l, err := journal.Create(m.Journal)
	if err != nil {
		return err
	}
	m.Log = l
	return nil
}

// attachLog opens the existing journal for append without truncating it.
func (m *Manager) attachLog() error {
	if m.Log != nil {
		return nil
	}
	l, err := journal.Open(m.Journal)
	if err != nil {
		return err
	}
	m.Log = l
	return nil
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return model.WrapError(model.StageJournal, model.CodePath, 0, 0, false, err, "lstat %s", path)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return model.NewError(model.StageJournal, model.CodePath, 0, 0, false,
			"refusing to write through symlink %s", path)
	}
	return nil
}

// State is the recovered transaction state.
type State struct {
	Records        []journal.Record
	CommittedStage int
	CommittedLen   int64
	RollbackBytes  int64
	Recovered      bool
}

// Recover validates the journal against the output file and truncates any
// uncommitted tail. It returns the last committed stage and length.
func Recover(dir, inputSHA string, inputSize int64, plan *mapping.Plan) (*State, error) {
	output, jpath := Paths(dir)
	recs, err := journal.Load(jpath)
	if err != nil {
		return nil, err
	}
	st := &State{CommittedStage: -1, Records: recs}
	lay := mux.PlanLayout(plan)
	info, err := os.Stat(output)
	if err != nil {
		return nil, model.WrapError(model.StageRecovery, model.CodeIntegrity, 0, 0, false, err,
			"stat output")
	}
	size := info.Size()
	stageCommits := map[int]journal.Record{}
	for i, r := range recs {
		if r.InputSHA256 != inputSHA || r.InputSize != inputSize {
			return nil, model.NewError(model.StageRecovery, model.CodeIntegrity, 0, 0, false,
				"journal record %d belongs to a different input", i)
		}
		if r.Kind != journal.KindCommit && r.Kind != journal.KindBegin {
			return nil, model.NewError(model.StageRecovery, model.CodeIntegrity, 0, 0, false,
				"journal record %d has unknown kind %q", i, r.Kind)
		}
		if r.Kind != journal.KindCommit {
			continue
		}
		if r.OutputLen > size {
			return nil, model.NewError(model.StageRecovery, model.CodeIntegrity, size, 0, false,
				"stage %d commit claims %d bytes but output is %d", r.Stage, r.OutputLen, size)
		}
		got, err := checksum.Prefix(output, r.OutputLen)
		if err != nil {
			return nil, model.WrapError(model.StageRecovery, model.CodeIntegrity, r.OutputLen, 0, false,
				err, "digest committed prefix")
		}
		if got != r.PrefixSHA {
			return nil, model.NewError(model.StageRecovery, model.CodeIntegrity, r.OutputLen, 0, false,
				"stage %d prefix digest mismatch: journal %s output %s", r.Stage, r.PrefixSHA, got)
		}
		stageCommits[r.Stage] = r
	}
	for s := 0; s <= len(plan.Tracks)+1; s++ {
		r, ok := stageCommits[s]
		if !ok {
			break
		}
		st.CommittedStage = s
		st.CommittedLen = r.OutputLen
	}
	if st.CommittedStage < 0 {
		// No commits at all: roll back to the ftyp/mdat header boundary.
		st.CommittedLen = lay.HeaderLen + 16
	}
	if size > st.CommittedLen {
		st.RollbackBytes = size - st.CommittedLen
		if err := truncateFile(output, st.CommittedLen); err != nil {
			return nil, err
		}
	}
	st.Recovered = true
	return st, nil
}

func truncateFile(path string, n int64) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return model.WrapError(model.StageRecovery, model.CodeIntegrity, n, 0, false, err, "open output")
	}
	if err := f.Truncate(n); err != nil {
		f.Close()
		return model.WrapError(model.StageRecovery, model.CodeIntegrity, n, 0, false, err, "truncate output")
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return model.WrapError(model.StageRecovery, model.CodeIntegrity, n, 0, false, err, "sync rollback")
	}
	return f.Close()
}

// Duration formats elapsed milliseconds with millisecond resolution.
func Duration(d time.Duration) int64 { return d.Milliseconds() }
