package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryAndContinue(t *testing.T) {
	transientCalls := 0
	failingCalls := 0
	tasks := []Task[string]{
		{Index: 0, Run: func(context.Context) (string, error) {
			transientCalls++
			if transientCalls < 2 {
				return "", errors.New("transient")
			}
			return "ok", nil
		}},
		{Index: 1, Run: func(context.Context) (string, error) {
			failingCalls++
			return "", errors.New("permanent")
		}},
		{Index: 2, Run: func(context.Context) (string, error) { return "ok", nil }},
	}
	outcomes := Run(context.Background(), 2, 3, time.Millisecond, tasks)
	if len(outcomes) != 3 {
		t.Fatalf("got %d outcomes", len(outcomes))
	}
	byIndex := map[int]Outcome[string]{}
	for _, out := range outcomes {
		byIndex[out.Index] = out
	}
	if byIndex[0].Err != nil || byIndex[0].Attempts != 2 || byIndex[2].Err != nil {
		t.Fatalf("transient task did not recover: %#v", byIndex)
	}
	if byIndex[1].Err == nil || byIndex[1].Attempts != 3 || failingCalls != 3 {
		t.Fatalf("permanent task did not exhaust retries: %#v", byIndex[1])
	}
}
