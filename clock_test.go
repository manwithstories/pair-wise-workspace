package pairflow_test

import (
	"testing"

	"pairflow/internal/clock"
)

func TestClockFileContract(t *testing.T) {
	u := clock.NewUnwrapper()
	if ext, _ := u.Observe(4294967000); ext != 4294967000 {
		t.Fatalf("initial ext=%d", ext)
	}
	ext, event := u.Observe(4000) // modular forward distance is 4296 ticks
	if event != nil {
		t.Fatal(event)
	}
	if ext != 4294971296 {
		t.Fatalf("wrapped ext=%d want 4294971296", ext)
	}
}
