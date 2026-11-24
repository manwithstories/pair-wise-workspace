package txn

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"mediad/internal/fault"
)

// StageState is one confirmed checkpoint output.
type StageState struct {
	Output string `json:"output"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	LSN    int64  `json:"lsn"`
}

// Checkpoint is the atomically replaced recent committed boundary.
type Checkpoint struct {
	Version   int                   `json:"version"`
	TxID      string                `json:"tx_id"`
	LastStage string                `json:"last_stage"`
	LastLSN   int64                 `json:"last_lsn"`
	Stages    map[string]StageState `json:"stages"`
	Checksum  string                `json:"checksum"`
}

type checkpointDigest struct {
	Version   int                   `json:"version"`
	TxID      string                `json:"tx_id"`
	LastStage string                `json:"last_stage"`
	LastLSN   int64                 `json:"last_lsn"`
	Stages    map[string]StageState `json:"stages"`
}

func (c *Checkpoint) sign() error {
	b, err := json.Marshal(checkpointDigest{
		Version: c.Version, TxID: c.TxID, LastStage: c.LastStage,
		LastLSN: c.LastLSN, Stages: c.Stages,
	})
	if err != nil {
		return err
	}
	c.Checksum = SHA256Bytes(b)
	return nil
}

func (c *Checkpoint) verify() error {
	want := c.Checksum
	if err := c.sign(); err != nil {
		return err
	}
	if c.Checksum != want {
		return fmt.Errorf("checkpoint checksum mismatch")
	}
	return nil
}

// Manager coordinates WAL, checkpoint and atomic stage outputs.
type Manager struct {
	Dir         string
	TxID        string
	WAL         *WAL
	CP          *Checkpoint
	Rollbacks   int
	CorruptTail bool
	InjectStage string
}

// OpenManager recovers the WAL and loads or rebuilds the checkpoint.
func OpenManager(dir, txID, injectStage string) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fault.Unrecoverable("txn", err)
	}
	walPath := filepath.Join(dir, WALFileName)
	beforeSize := int64(0)
	if st, err := os.Stat(walPath); err == nil {
		beforeSize = st.Size()
	}
	rawEntries, validBytes, err := readWAL(walPath)
	if err != nil {
		return nil, fault.Unrecoverable("wal-recovery", err)
	}
	corruptTail := beforeSize > validBytes

	w, err := OpenWAL(dir)
	if err != nil {
		return nil, fault.Unrecoverable("wal-open", err)
	}

	cp, err := loadCheckpoint(dir)
	if err != nil {
		cp = checkpointFromWAL(txID, rawEntries)
		if err := writeCheckpoint(dir, cp); err != nil {
			_ = w.Close()
			return nil, fault.Unrecoverable("checkpoint-rebuild", err)
		}
	}
	if cp.Stages == nil {
		cp.Stages = map[string]StageState{}
	}
	cp.TxID = txID

	m := &Manager{Dir: dir, TxID: txID, WAL: w, CP: cp, CorruptTail: corruptTail, InjectStage: injectStage}
	m.recoverIncomplete(rawEntries)
	return m, nil
}

func loadCheckpoint(dir string) (*Checkpoint, error) {
	b, err := os.ReadFile(filepath.Join(dir, CheckpointFileName))
	if errors.Is(err, os.ErrNotExist) {
		return &Checkpoint{Version: Version, Stages: map[string]StageState{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, fmt.Errorf("checkpoint decode: %w", err)
	}
	if err := cp.verify(); err != nil {
		return nil, err
	}
	if cp.Stages == nil {
		cp.Stages = map[string]StageState{}
	}
	return &cp, nil
}

func checkpointFromWAL(txID string, entries []Entry) *Checkpoint {
	cp := &Checkpoint{Version: Version, TxID: txID, Stages: map[string]StageState{}}
	for _, e := range entries {
		if e.Op != "commit" {
			continue
		}
		cp.LastStage = e.Stage
		cp.LastLSN = e.LSN
		cp.Stages[e.Stage] = StageState{
			Output: e.Output, SHA256: e.SHA256, Bytes: e.Bytes, LSN: e.LSN,
		}
	}
	return cp
}

func (m *Manager) recoverIncomplete(entries []Entry) {
	closed := map[string]bool{}
	committed := map[string]bool{}
	for _, e := range entries {
		key := e.TxID + "\x00" + e.Stage
		switch e.Op {
		case "commit", "rollback":
			closed[key] = true
			if e.Op == "commit" {
				committed[key] = true
			}
		}
	}
	seenBegin := map[string]bool{}
	for _, e := range entries {
		key := e.TxID + "\x00" + e.Stage
		if e.Op != "begin" || e.TxID != m.TxID || committed[key] {
			continue
		}
		if closed[key] || seenBegin[key] {
			// A prior recovery already closed this attempt, or only the newest
			// attempt needs rollback accounting.
			continue
		}
		seenBegin[key] = true
		m.Rollbacks++
		_, _ = m.WAL.Append(m.TxID, e.Stage, "rollback", "", "", 0)
	}
}

// IsCommitted reports whether stage has a checkpoint entry whose output still
// has the exact committed SHA-256.
func (m *Manager) IsCommitted(stage string) bool {
	st, ok := m.CP.Stages[stage]
	if !ok || st.SHA256 == "" || st.Output == "" {
		return false
	}
	sum, n, err := SHA256File(st.Output)
	if err != nil || n != st.Bytes || sum != st.SHA256 {
		return false
	}
	return true
}

// CommitStage atomically installs data at relPath and records the boundary.
func (m *Manager) CommitStage(stage, relPath string, data []byte) (StageState, error) {
	output := JoinOutput(m.Dir, relPath)
	if _, err := m.WAL.Append(m.TxID, stage, "begin", output, "", 0); err != nil {
		return StageState{}, fault.Unrecoverable("wal-begin", err)
	}
	if m.InjectStage == stage {
		return StageState{}, fault.Interrupted(stage, fmt.Errorf("injected failure before commit"))
	}
	if err := AtomicWrite(output, data); err != nil {
		return StageState{}, fault.Unrecoverable(stage, err)
	}
	sum := SHA256Bytes(data)
	if _, err := m.WAL.Append(m.TxID, stage, "commit", output, sum, int64(len(data))); err != nil {
		return StageState{}, fault.Unrecoverable("wal-commit", err)
	}
	st := StageState{Output: output, SHA256: sum, Bytes: int64(len(data)), LSN: m.WAL.lsn}
	m.CP.Version = Version
	m.CP.TxID = m.TxID
	m.CP.LastStage = stage
	m.CP.LastLSN = m.WAL.lsn
	if m.CP.Stages == nil {
		m.CP.Stages = map[string]StageState{}
	}
	m.CP.Stages[stage] = st
	if err := writeCheckpoint(m.Dir, m.CP); err != nil {
		return st, fault.Unrecoverable("checkpoint", err)
	}
	return st, nil
}

// Path returns an output path inside the transaction directory.
func (m *Manager) Path(rel string) string { return JoinOutput(m.Dir, rel) }

func writeCheckpoint(dir string, cp *Checkpoint) error {
	if err := cp.sign(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWrite(filepath.Join(dir, CheckpointFileName), b)
}

func (m *Manager) Close() error {
	if m.WAL == nil {
		return nil
	}
	return m.WAL.Close()
}
