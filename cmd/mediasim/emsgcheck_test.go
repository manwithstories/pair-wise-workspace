package main

import (
	"testing"

	"mediasim/internal/meta"
)

// A truncated emsg segment must surface a parse error, and a clean
// segment must round-trip every event.
func TestEmsgTruncationDetected(t *testing.T) {
	_, evs := genEvents()
	b, err := meta.BuildSegment(evs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := meta.ParseSegment(b[:len(b)-7]); err == nil {
		t.Fatal("truncated emsg segment parsed without error")
	}
	parsed, err := meta.ParseSegment(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != len(evs) {
		t.Fatalf("parsed %d events, want %d", len(parsed), len(evs))
	}
	for i, ev := range parsed {
		if ev.PresentationTime != evs[i].PresentationTime || ev.ID != evs[i].ID {
			t.Fatalf("event %d mismatch: got %+v want %+v", i, ev, evs[i])
		}
	}
}
