package txn

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"mediacheck/internal/journal"
)

// Output is one file published by a transaction.
type Output struct {
	Name    string
	Content []byte
}

// Txn coordinates atomic commits against a WAL and a work directory.
// Commit protocol: WAL begin (fsync) → write tmp files (fsync) →
// atomic rename into out/ → WAL committed (fsync). A crash before the
// committed record leaves a state Recover can finish or roll back.
type Txn struct {
	wal *journal.WAL
	dir string
}

// New creates a Txn rooted at dir (which holds tmp/ and out/).
func New(wal *journal.WAL, dir string) *Txn {
	return &Txn{wal: wal, dir: dir}
}

// Digest is the content digest of an output set, stable across runs.
func Digest(outs []Output) string {
	names := make([]string, 0, len(outs))
	byName := map[string][]byte{}
	for _, o := range outs {
		names = append(names, o.Name)
		byName[o.Name] = o.Content
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s\n", n)
		h.Write(byName[n])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Commit atomically publishes outs as transaction id.
func (t *Txn) Commit(id int, outs []Output) (string, error) {
	digest := Digest(outs)
	if _, err := t.wal.Append(journal.Record{
		Type: journal.TypeBegin, Txn: id, Output: digest,
		Detail: fmt.Sprintf("files=%d", len(outs)),
	}); err != nil {
		return "", err
	}
	tmp := filepath.Join(t.dir, "tmp", fmt.Sprintf("txn-%d", id))
	out := filepath.Join(t.dir, "out", fmt.Sprintf("txn-%d", id))
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", err
	}
	for _, o := range outs {
		p := filepath.Join(tmp, o.Name)
		if err := os.WriteFile(p, o.Content, 0o644); err != nil {
			return "", err
		}
		f, err := os.Open(p)
		if err != nil {
			return "", err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
	}
	if err := os.MkdirAll(filepath.Join(t.dir, "out"), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", err
	}
	if _, err := t.wal.Append(journal.Record{
		Type: journal.TypeCommitted, Txn: id, Output: digest,
	}); err != nil {
		return "", err
	}
	return digest, nil
}

// Recover inspects the last transaction after a crash:
//   - begin without committed, output dir complete → finish the commit
//     record (the rename already happened; never re-commit content);
//   - begin without committed, output missing/partial → roll back to
//     the last atomic boundary by removing tmp state.
//
// It returns what happened for the report.
func (t *Txn) Recover() (string, error) {
	begin, committed, found := t.wal.LastTxn()
	if !found {
		return "no-txn", nil
	}
	if committed {
		return "already-committed", nil
	}
	out := filepath.Join(t.dir, "out", fmt.Sprintf("txn-%d", begin.Txn))
	tmp := filepath.Join(t.dir, "tmp", fmt.Sprintf("txn-%d", begin.Txn))
	if _, err := os.Stat(out); err == nil {
		// Rename completed but the committed record was lost: the
		// content is durable, only the journal needs completion.
		if _, err := t.wal.Append(journal.Record{
			Type: journal.TypeCommitted, Txn: begin.Txn, Output: begin.Output,
			Detail: "completed-by-recovery",
		}); err != nil {
			return "", err
		}
		os.RemoveAll(tmp)
		return "completed", nil
	}
	// No durable output: roll back to the last atomic boundary.
	os.RemoveAll(tmp)
	if _, err := t.wal.Append(journal.Record{
		Type: journal.TypeRollback, Txn: begin.Txn, Detail: "incomplete-txn-discarded",
	}); err != nil {
		return "", err
	}
	return "rolled-back", nil
}
