package txn

import (
	"os"
	"path/filepath"
	"testing"

	"mediacheck/internal/journal"
)

func setup(t *testing.T) (*journal.WAL, *Txn, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return w, New(w, dir), dir
}

func TestCommitPublishesAtomically(t *testing.T) {
	w, tx, dir := setup(t)
	defer w.Close()
	outs := []Output{
		{Name: "a.txt", Content: []byte("alpha")},
		{Name: "b.txt", Content: []byte("beta")},
	}
	d, err := tx.Commit(1, outs)
	if err != nil {
		t.Fatal(err)
	}
	if d != Digest(outs) {
		t.Fatal("returned digest differs from Digest()")
	}
	got, err := os.ReadFile(filepath.Join(dir, "out", "txn-1", "a.txt"))
	if err != nil || string(got) != "alpha" {
		t.Fatalf("published content=%q err=%v", got, err)
	}
	// tmp directory must be gone after the rename.
	if _, err := os.Stat(filepath.Join(dir, "tmp", "txn-1")); !os.IsNotExist(err) {
		t.Fatal("tmp directory left behind after commit")
	}
	_, committed, found := w.LastTxn()
	if !found || !committed {
		t.Fatal("committed record missing")
	}
}

func TestRecoverWithoutTxn(t *testing.T) {
	w, tx, _ := setup(t)
	defer w.Close()
	action, err := tx.Recover()
	if err != nil || action != "no-txn" {
		t.Fatalf("action=%q err=%v", action, err)
	}
}

func TestRecoverRollsBackIncompleteTxn(t *testing.T) {
	w, _, dir := setup(t)
	// Simulate a crash after the begin record but before the rename:
	// write the begin record and a partial tmp tree manually.
	if _, err := w.Append(journal.Record{Type: journal.TypeBegin, Txn: 7, Output: "abc"}); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "tmp", "txn-7")
	os.MkdirAll(tmp, 0o755)
	os.WriteFile(filepath.Join(tmp, "partial.txt"), []byte("x"), 0o644)
	w.Close()

	// Reopen and recover.
	w2, _ := journal.Open(dir)
	defer w2.Close()
	action, err := New(w2, dir).Recover()
	if err != nil {
		t.Fatal(err)
	}
	if action != "rolled-back" {
		t.Fatalf("action=%q want rolled-back", action)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("partial tmp tree not removed")
	}
	// A rollback record closes the txn.
	_, committed, _ := w2.LastTxn()
	if committed {
		t.Fatal("rolled-back txn must not be marked committed")
	}
}

func TestRecoverCompletesRenamedTxn(t *testing.T) {
	w, _, dir := setup(t)
	// Crash after the rename but before the committed record.
	w.Append(journal.Record{Type: journal.TypeBegin, Txn: 3, Output: "ddd"})
	out := filepath.Join(dir, "out", "txn-3")
	os.MkdirAll(out, 0o755)
	os.WriteFile(filepath.Join(out, "seg.txt"), []byte("data"), 0o644)
	w.Close()

	w2, _ := journal.Open(dir)
	defer w2.Close()
	action, err := New(w2, dir).Recover()
	if err != nil {
		t.Fatal(err)
	}
	if action != "completed" {
		t.Fatalf("action=%q want completed", action)
	}
	_, committed, _ := w2.LastTxn()
	if !committed {
		t.Fatal("recovery did not complete the commit record")
	}
}

func TestRecoverAlreadyCommitted(t *testing.T) {
	w, tx, _ := setup(t)
	defer w.Close()
	tx.Commit(1, []Output{{Name: "a", Content: []byte("a")}})
	action, err := tx.Recover()
	if err != nil || action != "already-committed" {
		t.Fatalf("action=%q err=%v", action, err)
	}
}

func TestDigestStableAcrossOrder(t *testing.T) {
	a := []Output{{Name: "x", Content: []byte("1")}, {Name: "y", Content: []byte("2")}}
	b := []Output{{Name: "y", Content: []byte("2")}, {Name: "x", Content: []byte("1")}}
	if Digest(a) != Digest(b) {
		t.Fatal("digest depends on output order")
	}
}
