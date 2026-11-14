package replay

import (
	"path/filepath"
	"testing"
)

func TestLedgerReplayAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.CheckNew("dev-A", 1, "nonce-1"); err != nil {
		t.Fatal(err)
	}
	if err := l.Commit("dev-A", 1, "nonce-1"); err != nil {
		t.Fatal(err)
	}
	// 同凭据序号相等：Check 放行
	if err := l.Check("dev-A", 1); err != nil {
		t.Fatalf("equal sequence should pass: %v", err)
	}
	// 序号倒退 -> REPLAY
	if err := l.Check("dev-A", 0); err == nil {
		t.Fatal("want replay on rollback")
	}
	// 重开账本，重复 nonce -> REPLAY
	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l2.CheckNew("dev-A", 2, "nonce-1"); err == nil {
		t.Fatal("want replay on reused nonce after reopen")
	}
	if err := l2.CheckNew("dev-A", 2, "nonce-2"); err != nil {
		t.Fatalf("fresh nonce should pass: %v", err)
	}
}
