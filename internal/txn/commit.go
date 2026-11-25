package txn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pairwise/mediapack/internal/fingerprint"
	"github.com/pairwise/mediapack/internal/timeline"
)

var (
	// ErrCrashInjected is returned at the configured durable WAL point.
	ErrCrashInjected = errors.New("injected process crash")
	// ErrInvariant means recovery could not prove a committed result.
	ErrInvariant = errors.New("commit/recovery invariant failed")
)

// Manager atomically commits named artifacts under Dir/objects and replays Dir/wal.log.
type Manager struct {
	Dir       string
	ObjectDir string
	TempDir   string
	WALPath   string
	WAL       *WAL

	// CrashAfter, when positive, injects a crash after this many WAL records.
	CrashAfter int64
}

// RecoveryReport is the verifiable outcome of recovery.
type RecoveryReport struct {
	Committed         []Record
	PreparedRecovered []Record
	RolledBackTemp    []string
	TornTail          bool
}

// NewManager creates directories and opens the WAL.
func NewManager(dir string, crashAfter int64) (*Manager, error) {
	objectDir := filepath.Join(dir, "objects")
	tempDir := filepath.Join(dir, "tmp")
	for _, d := range []string{dir, objectDir, tempDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	walPath := filepath.Join(dir, "wal.log")
	wal, err := OpenWAL(walPath)
	if err != nil {
		return nil, err
	}
	return &Manager{
		Dir: dir, ObjectDir: objectDir, TempDir: tempDir,
		WALPath: walPath, WAL: wal, CrashAfter: crashAfter,
	}, nil
}

// Close flushes the WAL. Tests and CLIs call it on clean paths; a simulated
// crash returns ErrCrashInjected before the caller invokes normal follow-up.
func (m *Manager) Close() error {
	if m == nil || m.WAL == nil {
		return nil
	}
	return m.WAL.Close()
}

// Commit writes data to a temp file, checkpoints its checksum in the WAL,
// atomically renames it, and records the committed stage. Identical existing
// artifacts are idempotent and receive no duplicate WAL transaction.
func (m *Manager) Commit(txnID, name string, data []byte) (Record, error) {
	sha := fingerprintSHA(data)
	finalPath, err := m.safeObjectPath(name)
	if err != nil {
		return Record{}, err
	}

	if existingSHA, _, err := SHA256File(finalPath); err == nil {
		if existingSHA == sha {
			return Record{Stage: "existing", TxnID: txnID, Name: name, SHA256: sha, Size: len(data)}, nil
		}
		return Record{}, invariant("existing object hash mismatch for "+name, nil)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Record{}, err
	}

	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return Record{}, err
	}
	tmp, err := os.CreateTemp(m.TempDir, "commit-*.tmp")
	if err != nil {
		return Record{}, err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return Record{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return Record{}, err
	}
	if err := tmp.Close(); err != nil {
		return Record{}, err
	}
	gotSHA, _, err := SHA256File(tmpName)
	if err != nil {
		return Record{}, err
	}
	if gotSHA != sha {
		return Record{}, invariant("temp checksum mismatch for "+name, nil)
	}

	if _, err := m.WAL.Append("prepared", txnID, name, sha, len(data)); err != nil {
		return Record{}, err
	}
	if err := os.Rename(tmpName, finalPath); err != nil {
		return Record{}, err
	}
	cleanup = false

	rec, err := m.WAL.Append("committed", txnID, name, sha, len(data))
	if err != nil {
		return Record{}, err
	}
	if m.CrashAfter > 0 && m.WAL.Len() >= m.CrashAfter {
		return rec, ErrCrashInjected
	}
	return rec, nil
}

// Recover verifies committed WAL records against final objects and rolls back
// every temporary file outside the atomic rename boundary.
func (m *Manager) Recover() (RecoveryReport, error) {
	reply, err := ReplayWAL(m.WALPath)
	if err != nil {
		return RecoveryReport{}, err
	}
	report := RecoveryReport{TornTail: reply.TornTail}

	byName := map[string]Record{}
	for _, r := range reply.Records {
		if r.Stage == "committed" {
			byName[r.Name] = r
		}
	}
	for _, r := range reply.Records {
		switch r.Stage {
		case "committed":
			if err := verifyRecord(m, r); err != nil {
				return report, err
			}
			report.Committed = append(report.Committed, r)
		case "prepared":
			finalPath, err := m.safeObjectPath(r.Name)
			if err != nil {
				return report, err
			}
			if sha, _, err := SHA256File(finalPath); err == nil && sha == r.SHA256 {
				// The atomic rename was reached even if the final committed WAL
				// append was lost. The checksum proves the result.
				report.PreparedRecovered = append(report.PreparedRecovered, r)
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return report, err
			}
		}
	}

	entries, err := os.ReadDir(m.TempDir)
	if err != nil {
		return report, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		p := filepath.Join(m.TempDir, e.Name())
		if err := os.Remove(p); err != nil {
			return report, err
		}
		report.RolledBackTemp = append(report.RolledBackTemp, p)
	}
	return report, nil
}

// ObjectPath returns the verified final path for an artifact name.
func (m *Manager) ObjectPath(name string) (string, error) {
	return m.safeObjectPath(name)
}

func (m *Manager) safeObjectPath(name string) (string, error) {
	if name == "" || filepath.IsAbs(name) {
		return "", invariant("invalid artifact name "+name, nil)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", invariant("artifact name escapes object directory: "+name, nil)
	}
	p := filepath.Join(m.ObjectDir, clean)
	rel, err := filepath.Rel(m.ObjectDir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", invariant("artifact path escapes object directory: "+name, err)
	}
	return p, nil
}

func verifyRecord(m *Manager, r Record) error {
	p, err := m.safeObjectPath(r.Name)
	if err != nil {
		return err
	}
	sha, size, err := SHA256File(p)
	if err != nil {
		return invariant(fmt.Sprintf("committed object %s missing", r.Name), err)
	}
	if sha != r.SHA256 || int(size) != r.Size {
		return invariant(fmt.Sprintf("committed object %s checksum/size mismatch", r.Name), nil)
	}
	return nil
}

func invariant(msg string, err error) error {
	wrapped := fmt.Errorf("%w: %s", ErrInvariant, msg)
	if err != nil {
		wrapped = fmt.Errorf("%w: %v", wrapped, err)
	}
	return timeline.NewError(timeline.ErrTransaction, msg, wrapped)
}

func fingerprintSHA(data []byte) string {
	return fingerprint.ClipSHA256(data)
}
