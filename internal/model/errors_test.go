package model

import (
	"errors"
	"testing"

	"mediakernel/internal/clock"
)

func TestMediaErrorClassification(t *testing.T) {
	e := &MediaError{
		Category: CatConflict,
		Stream:   7,
		Seq:      42,
		At:       clock.Milliseconds(120),
		Detail:   "payload mismatch",
		Decision: DecIsolated,
	}
	var me *MediaError
	if !errors.As(e, &me) || me.Category != CatConflict {
		t.Fatal("error classification lost")
	}
	if me.Error() == "" {
		t.Fatal("empty error text")
	}
}

func TestLogBoundedAndCount(t *testing.T) {
	log := NewLog(3)
	for i := 0; i < 6; i++ {
		log.Add(MediaError{Category: CatLoss, Decision: DecSilenceFill})
	}
	if log.Len() != 3 {
		t.Fatalf("log cap not enforced: %d", log.Len())
	}
	if log.Evicted() != 3 {
		t.Fatalf("eviction count: %d", log.Evicted())
	}
	if log.CountBy(CatLoss) != 3 || log.CountBy(CatSync) != 0 {
		t.Fatal("category counts wrong")
	}
	got := log.Items()
	got[0].Category = CatSync
	if log.Items()[0].Category != CatLoss {
		t.Fatal("Items must return a copy")
	}
}

func TestPercentile(t *testing.T) {
	if Percentile(nil, 95) != 0 {
		t.Fatal("empty percentile must be zero")
	}
	v := []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if got := Percentile(v, 95); got != 100 {
		t.Fatalf("p95=%d", got)
	}
	if got := Percentile(v, 50); got < 50 || got > 60 {
		t.Fatalf("p50=%d", got)
	}
}
