package txn

import (
	"fmt"
	"os"
	"path/filepath"
)

// Stage names a pipeline phase whose artifacts are committed atomically.
type Stage string

const (
	// StageProbe covers container parsing and metadata normalisation.
	StageProbe Stage = "probe"
	// StageRTP covers packet reordering and loss classification.
	StageRTP Stage = "rtp"
	// StageRepair covers timestamp repair.
	StageRepair Stage = "repair"
	// StageIndex covers index rebuild and seek verification.
	StageIndex Stage = "index"
	// StageManifest covers manifest alignment and statistics.
	StageManifest Stage = "manifest"
)

// Order is the canonical pipeline order.
var Order = []Stage{StageProbe, StageRTP, StageRepair, StageIndex, StageManifest}

// index returns a stage's position in the pipeline, or -1.
func (s Stage) index() int {
	for i, v := range Order {
		if v == s {
			return i
		}
	}
	return -1
}

// Checkpoint records that a stage finished and which artifact it produced.
type Checkpoint struct {
	Stage    Stage  `json:"stage"`
	Artifact string `json:"artifact"`
	// SHA256 is the hex digest of the artifact's bytes at commit time.
	SHA256 string `json:"sha256"`
	// Bytes is the artifact size.
	Bytes int64 `json:"bytes"`
	// Seq is the WAL sequence of the commit record.
	Seq uint64 `json:"seq"`
}

// CommitError is returned when a rollback was performed.
type CommitError struct {
	RolledBackTo Stage
	Reason       string
}

// Error implements error.
func (e *CommitError) Error() string {
	if e.RolledBackTo == "" {
		return "commit failed: " + e.Reason
	}
	return fmt.Sprintf("commit failed, rolled back to %s: %s", e.RolledBackTo, e.Reason)
}

// Manager runs the stage protocol against one WAL.
type Manager struct {
	dir  string
	wal  *WAL
	scan ScanResult
	// maxStageBytes caps a single stage artifact.
	maxStageBytes int64
	// corrupt lists stages whose committed artifact failed verification.
	corrupt []Stage
	// rolledBack is the stage the last Recover rolled back to.
	rolledBack Stage
}

// Open prepares a manager rooted at dir, creating the directory and its WAL.
func Open(dir string, maxStageBytes int64) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	wal, scan, err := OpenWAL(filepath.Join(dir, "wal.log"))
	if err != nil {
		return nil, err
	}
	if maxStageBytes <= 0 {
		maxStageBytes = 64 << 20
	}
	m := &Manager{dir: dir, wal: wal, scan: scan, maxStageBytes: maxStageBytes}
	return m, nil
}

func (m *Manager) Close() error { return m.wal.Close() }

// WAL exposes the underlying log for callers that need to append their own
// records, such as fault-injection tests.
func (m *Manager) WAL() *WAL { return m.wal }

// AppendTornTail simulates a crash in the middle of a record write.
func (m *Manager) AppendTornTail() error { return m.wal.AppendTornTail() }

// Rescan refreshes the manager's view of the log after this process appended
// records, so Recover can evaluate the current contents rather than the
// snapshot taken at Open.
func (m *Manager) Rescan() error {
	res, err := m.wal.Rescan()
	if err != nil {
		return err
	}
	m.scan = res
	m.corrupt = nil
	return nil
}

func (m *Manager) RollbackStage() string { return string(m.rolledBack) }

// Scan exposes the WAL scan performed at open.
func (m *Manager) Scan() ScanResult { return m.scan }

// ValidPrefix returns how many bytes of the log were readable.
func (m *Manager) ValidPrefix() int64 { return m.scan.ValidBytes }

// CorruptTail reports whether a corrupt tail was found at open.
func (m *Manager) CorruptTail() (bool, string) {
	return m.scan.CorruptBytes > 0, m.scan.CorruptReason
}

func (m *Manager) ArtifactPath(s Stage) string {
	return filepath.Join(m.dir, fmt.Sprintf("%02d-%s.json", s.index(), s))
}
