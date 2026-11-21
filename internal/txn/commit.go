package txn

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"mediad/internal/model"
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

// Close closes the WAL.
func (m *Manager) Close() error { return m.wal.Close() }

// Scan exposes the WAL scan performed at open.
func (m *Manager) Scan() ScanResult { return m.scan }

// ValidPrefix returns how many bytes of the log were readable.
func (m *Manager) ValidPrefix() int64 { return m.scan.ValidBytes }

// CorruptTail reports whether a corrupt tail was found at open.
func (m *Manager) CorruptTail() (bool, string) {
	return m.scan.CorruptBytes > 0, m.scan.CorruptReason
}

// ArtifactPath returns the file a stage commits to.
func (m *Manager) ArtifactPath(s Stage) string {
	return filepath.Join(m.dir, fmt.Sprintf("%02d-%s.json", s.index(), s))
}

// Committed returns the stages whose artifacts are committed *and* still
// match their recorded checksum. A committed stage whose artifact was
// modified or deleted is not reusable.
func (m *Manager) Committed() map[Stage]Checkpoint {
	out := map[Stage]Checkpoint{}
	begun := map[Stage]bool{}
	pending := map[Stage]string{}
	for _, r := range m.scan.Records {
		s := Stage(r.Stage)
		switch r.Kind {
		case KindBegin:
			begun[s] = true
			delete(pending, s)
			delete(out, s)
		case KindData:
			pending[s] = r.Stage
		case KindCommit:
			if !begun[s] {
				continue
			}
			var cp Checkpoint
			if err := json.Unmarshal(r.Payload, &cp); err != nil {
				delete(out, s)
				continue
			}
			cp.Seq = r.Seq
			out[s] = cp
		case KindRollback:
			delete(out, s)
			delete(begun, s)
			delete(pending, s)
		}
	}
	// Drop anything whose artifact no longer matches.
	for s, cp := range out {
		ok, err := verifyArtifact(m.ArtifactPath(s), cp.SHA256)
		if err != nil || !ok {
			// Record why, so Recover can roll back rather than silently
			// treating the stage as merely unstarted.
			m.corrupt = append(m.corrupt, s)
			delete(out, s)
		}
	}
	return out
}

// ResumableFrom returns the first stage that has no valid commit, or
// StageManifest+1 when every stage is reusable.
func (m *Manager) ResumableFrom() Stage {
	done := m.Committed()
	for _, s := range Order {
		if _, ok := done[s]; !ok {
			return s
		}
	}
	return ""
}

// RollbackTo discards every stage after keep and records the rollback. The
// set of stages to discard comes from the WAL's commit records rather than
// from Committed(), because a commit whose artifact failed verification is
// exactly the case rollback must clean up.
func (m *Manager) RollbackTo(keep Stage, reason string) error {
	if keep != "" && keep.index() < 0 {
		return model.New(model.KindInvalidInput, "txn.commit", fmt.Sprintf("unknown stage %q", keep))
	}
	done := m.declaredCommits()
	var stages []Stage
	for s := range done {
		if keep != "" && s.index() <= keep.index() {
			continue
		}
		stages = append(stages, s)
	}
	sort.Slice(stages, func(i, j int) bool { return stages[i].index() < stages[j].index() })
	for i := len(stages) - 1; i >= 0; i-- {
		s := stages[i]
		if _, err := m.wal.Append(KindRollback, string(s), []byte(reason)); err != nil {
			return model.Wrap(model.KindRecovery, "txn.commit", "append rollback", err)
		}
		if err := os.Remove(m.ArtifactPath(s)); err != nil && !os.IsNotExist(err) {
			return model.Wrap(model.KindRecovery, "txn.commit", "remove artifact", err)
		}
	}
	return nil
}

// Commit writes the artifact for s and records its commit. The artifact is
// written to a temporary file, fsynced and renamed, so readers never see a
// partial stage.
func (m *Manager) Commit(s Stage, data []byte) (Checkpoint, error) {
	if s.index() < 0 {
		return Checkpoint{}, model.New(model.KindInvalidInput, "txn.commit", fmt.Sprintf("unknown stage %q", s))
	}
	if int64(len(data)) > m.maxStageBytes {
		return Checkpoint{}, model.New(model.KindOverLimit, "txn.commit",
			fmt.Sprintf("stage %s artifact %d bytes exceeds limit %d", s, len(data), m.maxStageBytes))
	}
	if _, err := m.wal.Append(KindBegin, string(s), nil); err != nil {
		return Checkpoint{}, model.Wrap(model.KindRecovery, "txn.commit", "append begin", err)
	}
	if _, err := m.wal.Append(KindData, string(s), data); err != nil {
		return Checkpoint{}, model.Wrap(model.KindRecovery, "txn.commit", "append data", err)
	}
	cp := Checkpoint{
		Stage:    s,
		Artifact: filepath.Base(m.ArtifactPath(s)),
		SHA256:   digest(data),
		Bytes:    int64(len(data)),
	}
	payload, err := json.Marshal(cp)
	if err != nil {
		return Checkpoint{}, model.Wrap(model.KindInternal, "txn.commit", "marshal checkpoint", err)
	}
	if _, err := m.wal.Append(KindCommit, string(s), payload); err != nil {
		return Checkpoint{}, model.Wrap(model.KindRecovery, "txn.commit", "append commit", err)
	}
	if err := writeFileAtomic(m.ArtifactPath(s), data); err != nil {
		return Checkpoint{}, model.Wrap(model.KindRecovery, "txn.commit", "write artifact", err)
	}
	return cp, nil
}

// Recover treats a corrupt WAL tail as a torn commit: the tail is truncated
// to the last valid record and the manager reports the stage to resume from.
// Corrupt artifacts are rolled back. A WAL whose header is unreadable cannot
// be recovered at all.
func (m *Manager) Recover() (resume Stage, err error) {
	if m.scan.CorruptBytes > 0 {
		if err := m.wal.TruncateTo(m.scan.ValidBytes); err != nil {
			return "", model.Wrap(model.KindRecovery, "txn.recover", "truncate corrupt tail", err)
		}
	}
	done := m.Committed()
	// Find the first stage whose artifact is missing or mismatched even
	// though the log claims a commit; everything after it is untrustworthy.
	for _, s := range Order {
		if _, ok := done[s]; ok {
			continue
		}
		if m.failedVerification(s) {
			// A commit record exists but the artifact is missing or its
			// digest does not match: the stage and everything after it is
			// untrustworthy, so roll back to the previous atomic boundary.
			prev := previous(s)
			if err := m.RollbackTo(prev, fmt.Sprintf("artifact for %s failed verification", s)); err != nil {
				return "", model.Wrap(model.KindRecovery, "txn.recover", "rollback", err)
			}
			return firstMissing(m.Committed()), nil
		}
		return s, nil
	}
	return "", nil
}

// declaredCommits returns the stages the WAL says were committed, whether or
// not their artifacts still verify.
func (m *Manager) declaredCommits() map[Stage]bool {
	out := map[Stage]bool{}
	for _, r := range m.scan.Records {
		if r.Kind == KindCommit {
			out[Stage(r.Stage)] = true
		}
	}
	return out
}

func (m *Manager) failedVerification(s Stage) bool {
	for _, c := range m.corrupt {
		if c == s {
			return true
		}
	}
	return false
}

func (m *Manager) declaredCommit(s Stage) bool {
	for _, r := range m.scan.Records {
		if Stage(r.Stage) == s && r.Kind == KindCommit {
			return true
		}
	}
	return false
}

func previous(s Stage) Stage {
	i := s.index()
	if i <= 0 {
		return ""
	}
	return Order[i-1]
}

func firstMissing(done map[Stage]Checkpoint) Stage {
	for _, s := range Order {
		if _, ok := done[s]; !ok {
			return s
		}
	}
	return ""
}

// digest returns the lowercase hex SHA-256 of data.
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

// VerifyArtifact checks a file against an expected digest.
func verifyArtifact(path, want string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return digest(data) == want, nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
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
	return os.Rename(tmp, path)
}
