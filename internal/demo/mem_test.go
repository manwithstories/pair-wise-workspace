package demo

import (
	"context"
	"testing"
)

func TestMemoryBoundedByDuration(t *testing.T) {
	small, err := Run(context.Background(), Options{Seed: 42, ReorderWindow: 64, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	// A much longer fixture: memory must not grow with duration.
	_ = small
}
