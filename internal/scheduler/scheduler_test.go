package scheduler

import (
	"errors"
	"testing"

	"archivecheck/internal/fault"
)

type out struct {
	Value string `json:"value"`
}

func chain(runB func(c *Ctx) (any, error)) []*Node {
	return []*Node{
		{Name: "a", Version: 1, Run: func(c *Ctx) (any, error) {
			return out{Value: "a"}, nil
		}},
		{Name: "b", Version: 1, Deps: []string{"a"}, Run: runB},
		{Name: "c", Version: 1, Deps: []string{"b"}, Run: func(c *Ctx) (any, error) {
			var b out
			if err := c.Dep("b", &b); err != nil {
				return nil, err
			}
			return out{Value: b.Value + "c"}, nil
		}},
	}
}

func build(t *testing.T, nodes []*Node) *Runner {
	t.Helper()
	r := New("params-1")
	for _, n := range nodes {
		if err := r.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func okB(c *Ctx) (any, error) {
	var a out
	if err := c.Dep("a", &a); err != nil {
		return nil, err
	}
	return out{Value: a.Value + "b"}, nil
}

func TestDAGOrderAndIdempotency(t *testing.T) {
	r := build(t, chain(okB))
	rep, err := r.Run("")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Completed || rep.RerunNodes != 3 || rep.SkippedNodes != 0 {
		t.Fatalf("first run: %+v", rep)
	}
	h := r.OutputHash("c")

	rep, err = r.Run("")
	if err != nil {
		t.Fatal(err)
	}
	if rep.RerunNodes != 0 || rep.SkippedNodes != 3 {
		t.Fatalf("second run must not re-produce: %+v", rep)
	}
	if r.OutputHash("c") != h {
		t.Fatal("output hash changed between identical runs")
	}
	var c out
	if err := r.Output("c", &c); err != nil || c.Value != "abc" {
		t.Fatalf("c = %+v err=%v", c, err)
	}
}

func TestFaultBlocksAndRetry(t *testing.T) {
	broken := true
	nodes := chain(func(c *Ctx) (any, error) {
		if broken {
			return nil, &fault.Fault{Kind: fault.KindUnsupportedLayout, Track: 2, Sample: -1, Offset: -1}
		}
		return okB(c)
	})
	r := build(t, nodes)
	rep, err := r.Run("")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Completed || rep.BlockedBy != "b" || rep.Fault.Kind != fault.KindUnsupportedLayout {
		t.Fatalf("blocked run: %+v", rep)
	}

	broken = false // repaired input: retry must resume from the failed node
	rep, err = r.Run("")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Completed || rep.RerunNodes != 2 || rep.SkippedNodes != 1 {
		t.Fatalf("retry: %+v", rep)
	}
}

func TestInterruptAndResumeFromCheckpoint(t *testing.T) {
	dir := t.TempDir()

	full := build(t, chain(okB))
	if _, err := full.Run(""); err != nil {
		t.Fatal(err)
	}
	wantHash := full.OutputHash("c")

	interrupted := build(t, chain(okB))
	rep, err := interrupted.Run("b")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Interrupted || rep.InterruptedAfter != "b" || rep.RerunNodes != 2 {
		t.Fatalf("interrupt: %+v", rep)
	}
	if err := interrupted.SaveState(dir); err != nil {
		t.Fatal(err)
	}

	resumed, err := LoadState(dir, "params-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range chain(okB) {
		if err := resumed.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	rep, err = resumed.Run("")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Completed || rep.RerunNodes != 1 || rep.SkippedNodes != 2 {
		t.Fatalf("resume: %+v", rep)
	}
	if resumed.OutputHash("c") != wantHash {
		t.Fatal("resumed checksum differs from uninterrupted run")
	}
}

func TestParamsMismatchRefusesResume(t *testing.T) {
	dir := t.TempDir()
	r := build(t, chain(okB))
	if _, err := r.Run("a"); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveState(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(dir, "other-params"); err == nil {
		t.Fatal("expected params mismatch error")
	}
}

func TestVersionBumpRerunsNodeAndDependents(t *testing.T) {
	nodes := chain(okB)
	r := build(t, nodes)
	if _, err := r.Run(""); err != nil {
		t.Fatal(err)
	}
	nodes[1].Version = 2 // b changed: b reruns; its output is unchanged,
	// so c's input hash is unchanged and c is correctly skipped.
	rep, err := r.Run("")
	if err != nil {
		t.Fatal(err)
	}
	if rep.RerunNodes != 1 || rep.SkippedNodes != 2 {
		t.Fatalf("version bump: %+v", rep)
	}
}

func TestDuplicateAndUnknownDeps(t *testing.T) {
	r := New("p")
	if err := r.Add(&Node{Name: "x", Run: okB}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(&Node{Name: "x", Run: okB}); err == nil {
		t.Fatal("duplicate node accepted")
	}
	if err := r.Add(&Node{Name: "y", Deps: []string{"nope"}, Run: okB}); err == nil {
		t.Fatal("unknown dependency accepted")
	}
}

func TestNonFaultErrorPropagates(t *testing.T) {
	r := build(t, []*Node{{Name: "a", Version: 1, Run: func(c *Ctx) (any, error) {
		return nil, errors.New("disk gone")
	}}})
	if _, err := r.Run(""); err == nil {
		t.Fatal("expected runtime error")
	}
}
