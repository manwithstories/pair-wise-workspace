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

func (m *Manager) ResumableFrom() Stage {
	done := m.Committed()
	for _, s := range Order {
		if _, ok := done[s]; !ok {
			return s
		}
	}
	return ""
}

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
			m.rolledBack = prev
			return firstMissing(m.Committed()), nil
		}
		return s, nil
	}
	return "", nil
}

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

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

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
