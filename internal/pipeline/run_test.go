package pipeline

import (
	"errors"
	"testing"

	"mediaroute/internal/model"
)

func stage(id string, deps []string, run func(*Context) error) Stage {
	return Stage{Node: model.Node{ID: id, Deps: deps}, Run: run}
}

func TestTopoExecutionOrder(t *testing.T) {
	var ran []string
	rec := func(id string) func(*Context) error {
		return func(*Context) error { ran = append(ran, id); return nil }
	}
	p, err := New(
		stage("inspect", []string{"sync"}, rec("inspect")),
		stage("sync", []string{"recovery"}, rec("sync")),
		stage("recovery", []string{"reorder"}, rec("recovery")),
		stage("reorder", []string{"manifest"}, rec("reorder")),
		stage("manifest", nil, rec("manifest")),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Run(NewContext()); err != nil {
		t.Fatal(err)
	}
	want := []string{"manifest", "reorder", "recovery", "sync", "inspect"}
	for i := range want {
		if ran[i] != want[i] {
			t.Fatalf("order %v want %v", ran, want)
		}
	}
}

func TestErrorStopsDownstreamCommits(t *testing.T) {
	committed := false
	gap := &model.MediaError{Code: model.ErrGap, Module: "recovery",
		Track: "v1", Seq: 18, Timestamp: 540000, Msg: "burst not covered"}
	p, err := New(
		stage("recovery", nil, func(*Context) error { return gap }),
		stage("sync", []string{"recovery"}, func(*Context) error {
			return syncStage(gap) // would propagate, but must never run
		}),
		stage("commit", []string{"sync"}, func(*Context) error {
			committed = true
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Run(NewContext())
	me, ok := err.(*model.MediaError)
	if !ok || me.Code != model.ErrGap || me.Seq != 18 {
		t.Fatalf("context lost: %v", err)
	}
	if committed {
		t.Fatal("downstream commit ran after failure")
	}
	if model.ExitCode(err) != model.ExitUnrecoverable {
		t.Fatalf("exit %d", model.ExitCode(err))
	}
}

// syncStage mimics the sync module annotating a recovery error.
func syncStage(err error) error {
	return err.(*model.MediaError).With("sync", "cannot align across gap")
}

func TestCycleAndMissingDepRejected(t *testing.T) {
	if _, err := New(
		stage("a", []string{"b"}, nil),
		stage("b", []string{"a"}, nil),
	); err == nil {
		t.Fatal("cycle must be rejected")
	}
	if _, err := New(stage("a", []string{"ghost"}, nil)); err == nil {
		t.Fatal("missing dep must be rejected")
	}
}

func TestPlainErrorWrapped(t *testing.T) {
	p, err := New(stage("x", nil, func(*Context) error { return errors.New("boom") }))
	if err != nil {
		t.Fatal(err)
	}
	err = p.Run(NewContext())
	me, ok := err.(*model.MediaError)
	if !ok || me.Code != model.ErrInternal {
		t.Fatalf("want internal error, got %v", err)
	}
}
