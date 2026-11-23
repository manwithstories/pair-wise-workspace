package dag

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"medpipe/internal/model"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "state", "nodes.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fixedRun(tag string, calls *int32, failFirst int32, failErr error) func(string) (Summary, error) {
	var n int32
	return func(key string) (Summary, error) {
		atomic.AddInt32(calls, 1)
		cur := atomic.AddInt32(&n, 1)
		if failFirst > 0 && cur <= failFirst {
			return Summary{}, failErr
		}
		return Summary{Digest: digestOf(tag + key), Detail: tag}, nil
	}
}

func digestOf(s string) [32]byte {
	var d [32]byte
	copy(d[:], s)
	return d
}

func TestTopoOrderAndValidation(t *testing.T) {
	store := tempStore(t)
	var calls int32
	nodes := []*Node{
		{Name: "fingerprint", Deps: []string{"cut"}, Run: fixedRun("f", &calls, 0, nil)},
		{Name: "cut", Deps: []string{"read"}, Run: fixedRun("c", &calls, 0, nil)},
		{Name: "read", Run: fixedRun("r", &calls, 0, nil)},
	}
	s, err := New(nodes, store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"read", "cut", "fingerprint"}
	got := s.Order()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if _, err := New([]*Node{{Name: "a", Deps: []string{"ghost"}, Run: fixedRun("x", &calls, 0, nil)}}, store, Options{}); err == nil {
		t.Fatal("expected unknown dependency error")
	}
	if _, err := New([]*Node{
		{Name: "a", Deps: []string{"b"}, Run: fixedRun("x", &calls, 0, nil)},
		{Name: "b", Deps: []string{"a"}, Run: fixedRun("x", &calls, 0, nil)},
	}, store, Options{}); err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestRunReusesPersistedResults(t *testing.T) {
	store := tempStore(t)
	var calls int32
	build := func() *Scheduler {
		s, err := New([]*Node{
			{Name: "read", Params: map[string]any{"seed": 1}, Run: fixedRun("r", &calls, 0, nil)},
			{Name: "cut", Deps: []string{"read"}, Run: fixedRun("c", &calls, 0, nil)},
		}, store, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	res, err := build().Run()
	if err != nil {
		t.Fatal(err)
	}
	if res.ExecutedNodes != 2 || res.ReusedNodes != 0 {
		t.Fatalf("first run executed=%d reused=%d", res.ExecutedNodes, res.ReusedNodes)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("run calls = %d, want 2", got)
	}
	res2, err := build().Run()
	if err != nil {
		t.Fatal(err)
	}
	if res2.ReusedNodes != 2 || res2.ExecutedNodes != 0 {
		t.Fatalf("second run executed=%d reused=%d", res2.ExecutedNodes, res2.ReusedNodes)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("cache miss: run calls = %d, want 2", got)
	}
	if store.Len() != 2 {
		t.Fatalf("store records = %d, want 2 (retries must not append)", store.Len())
	}
}

func TestParamsChangeInvalidatesKey(t *testing.T) {
	store := tempStore(t)
	var calls int32
	mk := func(window int) *Scheduler {
		s, _ := New([]*Node{{Name: "cut", Params: map[string]any{"window": window}, Run: fixedRun("c", &calls, 0, nil)}}, store, Options{})
		return s
	}
	a, err := mk(1).Run()
	if err != nil {
		t.Fatal(err)
	}
	b, err := mk(2).Run()
	if err != nil {
		t.Fatal(err)
	}
	if a.Results["cut"].Key == b.Results["cut"].Key {
		t.Fatal("parameter change must change the node key")
	}
	if b.ExecutedNodes != 1 || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("parameter change did not re-execute: %+v", b)
	}
}

func TestBlockedDependencyReported(t *testing.T) {
	store := tempStore(t)
	var calls int32
	failure := model.Corruption("cut", "cam0", 3, 512, "checksum mismatch")
	s, err := New([]*Node{
		{Name: "read", Run: fixedRun("r", &calls, 0, nil)},
		{Name: "cut", Deps: []string{"read"}, Run: func(string) (Summary, error) { return Summary{}, failure }},
		{Name: "fingerprint", Deps: []string{"cut"}, Run: fixedRun("f", &calls, 0, nil)},
		{Name: "silence", Deps: []string{"cut"}, Run: fixedRun("s", &calls, 0, nil)},
	}, store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Results["cut"].State != StateFailed {
		t.Fatalf("cut state = %s", res.Results["cut"].State)
	}
	if res.Results["fingerprint"].State != StateBlocked || res.Results["silence"].State != StateBlocked {
		t.Fatalf("successors not blocked: %+v", res.Results)
	}
	if res.FirstBlocked == nil || res.FirstBlocked.Node != "fingerprint" || res.FirstBlocked.Dep != "cut" {
		t.Fatalf("first blocked = %+v", res.FirstBlocked)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("blocked successors ran: calls = %d", calls)
	}
}

func TestRetryIsIdempotent(t *testing.T) {
	store := tempStore(t)
	var calls int32
	transient := errors.New("transient disk hiccup")
	s, err := New([]*Node{
		{Name: "read", Run: fixedRun("r", &calls, 1, transient)},
	}, store, Options{MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Results["read"].State != StateDone {
		t.Fatalf("state = %s, want done after retry", res.Results["read"].State)
	}
	if store.Len() != 1 {
		t.Fatalf("store records = %d, want 1", store.Len())
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("calls = %d, want 2 (one failure, one success)", calls)
	}
}

func TestCorruptFailureIsNotRetried(t *testing.T) {
	store := tempStore(t)
	var calls int32
	corrupt := model.Corruption("read", "cam0", 1, 64, "checksum mismatch")
	s, _ := New([]*Node{
		{Name: "read", Run: func(string) (Summary, error) { atomic.AddInt32(&calls, 1); return Summary{}, corrupt }},
	}, store, Options{MaxAttempts: 5})
	res, err := s.Run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Results["read"].State != StateFailed {
		t.Fatalf("state = %s", res.Results["read"].State)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("corrupt input retried %d times, want 1", got)
	}
}

func TestConcurrentDuplicateSubmission(t *testing.T) {
	store := tempStore(t)
	var calls int32
	var barrier sync.WaitGroup
	barrier.Add(1)
	run := func(string) (Summary, error) {
		atomic.AddInt32(&calls, 1)
		barrier.Wait() // hold the first execution open so others pile up
		return Summary{Digest: digestOf("same"), Detail: "ok"}, nil
	}
	s, err := New([]*Node{{Name: "read", Run: run}}, store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	results := make([]*RunResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.Run()
		}(i)
	}
	// Let one goroutine enter Run before releasing the barrier.
	for atomic.LoadInt32(&calls) == 0 {
		// spin until the executor is inside Run
	}
	barrier.Done()
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("submit %d: %v", i, errs[i])
		}
		if results[i].Results["read"].State != StateDone && results[i].Results["read"].State != StateReused {
			t.Fatalf("submit %d state = %s", i, results[i].Results["read"].State)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("run executed %d times for identical submissions, want 1", got)
	}
	if store.Len() != 1 {
		t.Fatalf("store records = %d, want 1", store.Len())
	}
}

func TestRecoveryFromFailedNode(t *testing.T) {
	store := tempStore(t)
	var bad atomic.Bool
	bad.Store(true)
	flaky := func(key string) (Summary, error) {
		if bad.Load() {
			return Summary{}, model.Corruption("read", "cam0", 2, 128, "checksum mismatch")
		}
		return Summary{Digest: digestOf("good" + key)}, nil
	}
	build := func() *Scheduler {
		var calls int32
		s, err := New([]*Node{
			{Name: "read", Run: flaky},
			{Name: "cut", Deps: []string{"read"}, Run: fixedRun("c", &calls, 0, nil)},
		}, store, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	res, err := build().Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.FailedNodes) != 1 || res.FailedNodes[0] != "read" {
		t.Fatalf("failed nodes = %v", res.FailedNodes)
	}
	bad.Store(false)
	res2, err := build().Run()
	if err != nil {
		t.Fatal(err)
	}
	if res2.Results["read"].State != StateDone || res2.Results["cut"].State != StateDone {
		t.Fatalf("recovery did not complete: %+v", res2.Results)
	}
	if res2.ExecutedNodes != 2 {
		t.Fatalf("executed = %d, want 2 after repair", res2.ExecutedNodes)
	}
}

func TestStoreRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nodes.json")
	if err := writeFile(path, "{not json"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path); err == nil {
		t.Fatal("expected corrupt store error")
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func TestKeyStableAcrossInputOrder(t *testing.T) {
	a, err := Key("n", InputDigests{"b", "a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Key("n", InputDigests{"a", "b"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("key depends on input order")
	}
	if _, err := Key("n", nil, func() {}); err == nil {
		t.Fatal("expected non-serializable parameter error")
	}
}
