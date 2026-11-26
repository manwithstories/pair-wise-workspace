package dag

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"mediaqc/internal/media"
)

func testGraph(j *Journal, bRun func(context.Context, map[string][]byte) ([]byte, error)) *Scheduler {
	s := NewScheduler(j)
	_ = s.Add(&Node{ID: "a", Input: []byte("root"), Run: func(context.Context, map[string][]byte) ([]byte, error) {
		return []byte("A"), nil
	}})
	_ = s.Add(&Node{ID: "b", Deps: []string{"a"}, Run: bRun})
	_ = s.Add(&Node{ID: "c", Deps: []string{"b"}, Run: func(_ context.Context, d map[string][]byte) ([]byte, error) {
		return append([]byte("C:"), d["b"]...), nil
	}})
	return s
}

func TestSchedulerIdempotentDuplicateRun(t *testing.T) {
	j := NewJournal()
	calls := 0
	runB := func(_ context.Context, d map[string][]byte) ([]byte, error) {
		calls++
		return append([]byte("B:"), d["a"]...), nil
	}
	s := testGraph(j, runB)
	ctx := context.Background()

	r1, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hash1 := HashBytes(r1.Outputs["c"])
	if calls != 1 || string(r1.Outputs["c"]) != "C:B:A" {
		t.Fatalf("calls=%d output=%q", calls, r1.Outputs["c"])
	}

	r2, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hash2 := HashBytes(r2.Outputs["c"])
	if hash1 != hash2 {
		t.Fatalf("hash changed: %s vs %s", hash1, hash2)
	}
	if calls != 1 || len(r2.Cached) != 3 {
		t.Fatalf("duplicate run must use journal: calls=%d cached=%v", calls, r2.Cached)
	}
}

func TestJournalPersistsAndRestoresIdempotentResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	j := NewJournal()
	s := testGraph(j, func(context.Context, map[string][]byte) ([]byte, error) { return []byte("B"), nil })
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := j.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	s2 := testGraph(loaded, func(context.Context, map[string][]byte) ([]byte, error) {
		t.Fatal("node should be served from journal")
		return nil, nil
	})
	r, err := s2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Outputs["c"]) != "C:B" || len(r.Cached) != 3 {
		t.Fatalf("output=%q cached=%v", r.Outputs["c"], r.Cached)
	}
}

func TestFailOnceRetriesAndReportsBlockedDependency(t *testing.T) {
	calls := 0
	j := NewJournal()
	s := testGraph(j, func(context.Context, map[string][]byte) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, media.New(media.KindInternal, "audio.repair", "injected failure")
		}
		return []byte("repaired"), nil
	})
	rep, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("retry should recover: %v", err)
	}
	if calls != 2 || len(rep.Retried) != 1 || rep.Retried[0] != "b" {
		t.Fatalf("calls=%d retried=%v", calls, rep.Retried)
	}
	if rep.FirstBlocked != "b" || len(rep.Blocked) != 1 || rep.Blocked[0] != "c" {
		t.Fatalf("first_blocked=%q blocked=%v", rep.FirstBlocked, rep.Blocked)
	}
	var sawRetrySuccess bool
	for _, e := range rep.Events {
		if e.Kind == "retry_success" && e.NodeID == "b" {
			sawRetrySuccess = true
		}
	}
	if !sawRetrySuccess {
		t.Fatalf("missing retry_success event: %+v", rep.Events)
	}
}

func TestPermanentFailureReportsBlockerAndKind(t *testing.T) {
	j := NewJournal()
	s := NewScheduler(j)
	_ = s.Add(&Node{ID: "bad", MaxAttempts: 1, Run: func(context.Context, map[string][]byte) ([]byte, error) {
		return nil, media.New(media.KindMalformed, "node", "truncated")
	}})
	_ = s.Add(&Node{ID: "child", Deps: []string{"bad"}, Run: func(context.Context, map[string][]byte) ([]byte, error) {
		return nil, errors.New("should not run")
	}})
	rep, err := s.Run(context.Background())
	if err == nil || media.Classify(err) != media.KindMalformed {
		t.Fatalf("err=%v kind=%s", err, media.Classify(err))
	}
	if rep.FirstBlocked != "bad" || len(rep.Blocked) != 1 || rep.Blocked[0] != "child" {
		t.Fatalf("report=%+v", rep)
	}
}

func TestInputHashStableAndSensitive(t *testing.T) {
	h1 := InputHash("n", []byte("x"), map[string]string{"a": "h"})
	h2 := InputHash("n", []byte("x"), map[string]string{"a": "h"})
	h3 := InputHash("n", []byte("y"), map[string]string{"a": "h"})
	if h1 != h2 || h1 == h3 {
		t.Fatal("input hash must be stable and change with input")
	}
}
