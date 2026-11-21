package dag

import (
	"errors"
	"testing"

	"mediapack/internal/fault"
)

func TestAllDone(t *testing.T) {
	st := stateAt(t)
	st.Nodes["a"] = Record{Status: StatusDone}
	st.Nodes["b"] = Record{Status: StatusPending}
	if AllDone(st, []string{"a", "b"}) {
		t.Fatal("pending node counted as done")
	}
	st.Nodes["b"] = Record{Status: StatusDone}
	if !AllDone(st, []string{"a", "b"}) {
		t.Fatal("done nodes reported unfinished")
	}
}

func TestWithRetryRetriesTransientOnly(t *testing.T) {
	attempts := 0
	n := Node{Name: "n", Run: func() (string, error) {
		attempts++
		if attempts < 2 {
			return "", fault.New(fault.ClassInternal, "dag", "n", "transient", "try again")
		}
		return "ok", nil
	}}
	out, err := WithRetry(n, DefaultRetry()).Run()
	if err != nil || out != "ok" || attempts != 2 {
		t.Fatalf("out=%q err=%v attempts=%d", out, err, attempts)
	}

	attempts = 0
	deterministic := Node{Name: "n", Run: func() (string, error) {
		attempts++
		return "", fault.New(fault.ClassInput, "dag", "n", "bad-input", "will not change")
	}}
	if _, err := WithRetry(deterministic, DefaultRetry()).Run(); err == nil {
		t.Fatal("input failure disappeared")
	}
	if attempts != 1 {
		t.Fatalf("deterministic failure retried %d times", attempts)
	}
}

func TestWithRetryExhaustsAndKeepsKey(t *testing.T) {
	attempts := 0
	n := Node{Name: "n", Inputs: []string{"i"}, Config: "c", Run: func() (string, error) {
		attempts++
		return "", errors.New("flaky")
	}}
	wrapped := WithRetry(n, RetryPolicy{MaxAttempts: 3})
	if wrapped.Key() != n.Key() {
		t.Fatal("wrapping changed the idempotency key")
	}
	if _, err := wrapped.Run(); err == nil {
		t.Fatal("expected failure")
	}
	if attempts != 3 {
		t.Fatalf("attempts=%d want 3", attempts)
	}
}

func TestBlockedChainWalksToEarliestBlocker(t *testing.T) {
	st := stateAt(t)
	st.Nodes["a"] = Record{Key: "k", Status: StatusFailed}
	st.Nodes["b"] = Record{Key: "k", Status: StatusBlocked, Blocked: "a"}
	st.Nodes["c"] = Record{Key: "k", Status: StatusBlocked, Blocked: "b"}
	chain, err := BlockedChain(st, "c")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 3 || chain[0] != "a" || chain[2] != "c" {
		t.Fatalf("chain=%v", chain)
	}
	if _, err := BlockedChain(st, "ghost"); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("unknown node: %v", err)
	}
}

func TestEarliestBlockedUsesExecutionOrder(t *testing.T) {
	st := stateAt(t)
	st.Nodes["b"] = Record{Key: "k", Status: StatusFailed}
	st.Nodes["c"] = Record{Key: "k", Status: StatusBlocked, Blocked: "b"}
	fe := EarliestBlocked(st, []string{"a", "b", "c"})
	if fe == nil || fe.BlockedBy != "dag:b" || fe.Track != "b" {
		t.Fatalf("got %+v", fe)
	}
	if got := EarliestBlocked(st, []string{"a"}); got != nil {
		t.Fatalf("returned %+v with no blocked nodes", got)
	}
}
