package txn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"mediamix/internal/clock"
)

// CommitManifest is the durable record of one committed boundary.
type CommitManifest struct {
	Seq       uint64 `json:"seq"`        // monotonic commit sequence
	RecordIDs []uint64 `json:"records"`  // WAL data record IDs in this commit
	Bytes     int    `json:"bytes"`      // payload bytes committed
	SHA256    string `json:"sha256"`     // hash over the concatenated payload
	Stage     string `json:"stage"`      // pipeline stage that produced it
}

// Commit coordinates WAL records with on-disk atomic commits. A commit is
// durable only after its manifest is renamed into place on the same
// filesystem (rename is atomic) and fsynced.
type Commit struct {
	dir          string
	wal          *WAL
	seq          uint64
	pending      []Record // data records staged since the last commit
	committed    []CommitManifest
	DroppedBytes int64 // corrupt tail bytes removed at open, for diagnostics
}

// Open prepares a commit coordinator in dir, opening and validating the WAL.
func Open(dir string) (*Commit, error) {
	wal, dropped, err := OpenWAL(dir)
	if err != nil {
		return nil, err
	}
	c := &Commit{dir: dir, wal: wal, DroppedBytes: dropped}
	if err := c.loadCommits(); err != nil {
		return nil, err
	}
	return c, nil
}

// Stage appends a data record to the WAL. The record is durable before this
// returns, but not visible as output until Commit succeeds.
func (c *Commit) Stage(stage string, payload []byte) (Record, error) {
	rec, err := c.wal.Append(KindData, payload)
	if err != nil {
		return Record{}, err
	}
	c.pending = append(c.pending, rec)
	return rec, nil
}

// Commit atomically publishes all staged records as one boundary. The
// manifest is written to a temp file and renamed, so a crash can never leave
// a half-written manifest that looks like a complete delivery.
func (c *Commit) Commit(stage string) (CommitManifest, error) {
	if len(c.pending) == 0 {
		return CommitManifest{}, clock.NewError(clock.StageCommit, clock.ErrInvalidParam, 0, false,
			"cannot commit an empty transaction")
	}
	c.seq++
	m := CommitManifest{Seq: c.seq, Stage: stage}
	h := sha256.New()
	for _, rec := range c.pending {
		m.RecordIDs = append(m.RecordIDs, rec.ID)
		m.Bytes += len(rec.Data)
		h.Write(rec.Data)
	}
	m.SHA256 = hex.EncodeToString(h.Sum(nil))

	final := filepath.Join(c.dir, manifestName(c.seq))
	tmp := final + ".tmp"
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		c.seq--
		return CommitManifest{}, clock.WrapError(clock.StageCommit, clock.ErrInvalidParam, 0, false,
			"encode manifest", err)
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		c.seq--
		return CommitManifest{}, clock.WrapError(clock.StageCommit, clock.ErrRollback, 0, false,
			"write temp manifest", err)
	}
	if err := syncDir(c.dir); err != nil {
		_ = os.Remove(tmp)
		c.seq--
		return CommitManifest{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		c.seq--
		return CommitManifest{}, clock.WrapError(clock.StageCommit, clock.ErrRollback, 0, false,
			"atomic rename manifest", err)
	}
	if err := syncDir(c.dir); err != nil {
		return CommitManifest{}, err
	}
	// Commit marker in the WAL itself; a later replay can prove the boundary.
	if _, err := c.wal.Append(KindCommit, data); err != nil {
		return CommitManifest{}, err
	}
	c.committed = append(c.committed, m)
	c.pending = nil
	return m, nil
}

// Rollback discards all staged, uncommitted records by writing an abort
// marker and clearing the pending set. Already committed boundaries are
// untouched, which is exactly the guarantee the pipeline relies on after an
// injected fault.
func (c *Commit) Rollback(reason string) error {
	marker, _ := json.Marshal(map[string]string{"abort": reason})
	if _, err := c.wal.Append(KindAbort, marker); err != nil {
		return err
	}
	c.pending = nil
	return nil
}

// Replay rebuilds state from the WAL and committed manifests, returning the
// manifests in commit order. Uncommitted data records past the last commit
// are excluded, so recovered output matches the last good boundary.
func (c *Commit) Replay() ([]CommitManifest, error) {
	records, err := c.wal.Replay()
	if err != nil {
		return nil, clock.WrapError(clock.StageCommit, clock.ErrReplay, 0, false, "read wal", err)
	}
	// Cross-check: every committed manifest's record IDs and hash must match
	// the data records actually present in the WAL.
	dataByID := make(map[uint64][]byte)
	var lastCommitSeen int
	for _, r := range records {
		switch r.Kind {
		case KindData:
			dataByID[r.ID] = r.Data
		case KindCommit:
			var m CommitManifest
			if err := json.Unmarshal(r.Data, &m); err != nil {
				return nil, clock.WrapError(clock.StageCommit, clock.ErrReplay, 0, false,
					"decode commit marker", err)
			}
			lastCommitSeen++
			if err := c.verifyManifest(m, dataByID); err != nil {
				return nil, err
			}
		case KindAbort:
			// Aborted span: nothing to restore; the pending data after it is
			// simply not referenced by any later commit.
		}
	}
	if len(c.committed) != lastCommitSeen {
		return nil, clock.NewError(clock.StageCommit, clock.ErrReplay, 0, false,
			"manifest count disagrees with WAL commit markers")
	}
	return append([]CommitManifest(nil), c.committed...), nil
}

func (c *Commit) verifyManifest(m CommitManifest, dataByID map[uint64][]byte) error {
	h := sha256.New()
	bytes := 0
	for _, id := range m.RecordIDs {
		data, ok := dataByID[id]
		if !ok {
			return clock.NewError(clock.StageCommit, clock.ErrReplay, 0, false,
				"committed record missing from WAL")
		}
		h.Write(data)
		bytes += len(data)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != m.SHA256 {
		return clock.NewError(clock.StageCommit, clock.ErrReplay, 0, false,
			"replay hash mismatch at seq "+itoa(m.Seq))
	}
	if bytes != m.Bytes {
		return clock.NewError(clock.StageCommit, clock.ErrReplay, 0, false,
			"replay byte count mismatch at seq "+itoa(m.Seq))
	}
	return nil
}

// Committed returns the manifests published so far.
func (c *Commit) Committed() []CommitManifest {
	return append([]CommitManifest(nil), c.committed...)
}

// CommittedPayload reconstructs the exact byte stream covered by every
// committed manifest, in commit order, by reading WAL data records. Records
// that were staged but never committed (or aborted) are excluded.
func (c *Commit) CommittedPayload() ([]byte, error) {
	records, err := c.wal.Replay()
	if err != nil {
		return nil, clock.WrapError(clock.StageCommit, clock.ErrReplay, 0, false,
			"read wal for payload", err)
	}
	dataByID := make(map[uint64][]byte, len(records))
	for _, rec := range records {
		if rec.Kind == KindData {
			dataByID[rec.ID] = rec.Data
		}
	}
	var out []byte
	for _, m := range c.committed {
		for _, id := range m.RecordIDs {
			data, ok := dataByID[id]
			if !ok {
				return nil, clock.NewError(clock.StageCommit, clock.ErrReplay, 0, false,
					"committed record "+itoa(id)+" missing during payload rebuild")
			}
			out = append(out, data...)
		}
	}
	return out, nil
}

// PendingCount reports staged-but-uncommitted records.
func (c *Commit) PendingCount() int { return len(c.pending) }

// Close releases the underlying WAL.
func (c *Commit) Close() error { return c.wal.Close() }

func (c *Commit) loadCommits() error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return clock.WrapError(clock.StageCommit, clock.ErrInvalidPath, 0, false, "read dir", err)
	}
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != commitSuffix {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(c.dir, name))
		if err != nil {
			return clock.WrapError(clock.StageCommit, clock.ErrReplay, 0, false, "read manifest", err)
		}
		var m CommitManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return clock.WrapError(clock.StageCommit, clock.ErrReplay, 0, false, "decode manifest", err)
		}
		c.committed = append(c.committed, m)
		if m.Seq > c.seq {
			c.seq = m.Seq
		}
	}
	// Order manifests by sequence.
	for i := 0; i < len(c.committed); i++ {
		for j := i + 1; j < len(c.committed); j++ {
			if c.committed[j].Seq < c.committed[i].Seq {
				c.committed[i], c.committed[j] = c.committed[j], c.committed[i]
			}
		}
	}
	return nil
}

func manifestName(seq uint64) string {
	return "commit-" + itoa(seq) + commitSuffix
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return clock.WrapError(clock.StageCommit, clock.ErrRollback, 0, false, "open dir for fsync", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return clock.WrapError(clock.StageCommit, clock.ErrRollback, 0, false, "fsync dir", err)
	}
	return nil
}
