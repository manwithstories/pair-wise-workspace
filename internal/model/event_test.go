package model

import (
	"encoding/json"
	"errors"
	"testing"

	"mediarun/internal/clock"
)

func TestKindStringRoundTrip(t *testing.T) {
	for _, k := range []Kind{KindSubtitle, KindChapter, KindID3, KindEmsg, KindSEI, KindFrame} {
		got, err := ParseKind(k.String())
		if err != nil || got != k {
			t.Fatalf("%v -> %v %v", k, got, err)
		}
	}
	if _, err := ParseKind("bogus"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bogus kind: %v", err)
	}
	if KindUnknown.String() != "unknown" {
		t.Fatal("unknown kind label")
	}
}

func TestEventEndOverflow(t *testing.T) {
	e := Event{Start: 1<<63 - 1, Duration: 2}
	if _, err := e.End(); !errors.Is(err, clock.ErrOverflow) {
		t.Fatalf("want overflow, got %v", err)
	}
	ok := Event{Start: 10, Duration: 5}
	end, err := ok.End()
	if err != nil || end != 15 {
		t.Fatalf("end=%d err=%v", end, err)
	}
}

func TestLessDeterministic(t *testing.T) {
	a := Event{Start: 100, Kind: KindSubtitle, TrackID: 2, Sequence: 1}
	b := Event{Start: 100, Kind: KindSubtitle, TrackID: 1, Sequence: 9}
	c := Event{Start: 100, Kind: KindEmsg, TrackID: 1, Sequence: 1}
	events := []Event{a, b, c}
	SortStable(events)
	if events[0].TrackID != 1 || events[1].TrackID != 2 || events[2].Kind != KindEmsg {
		t.Fatalf("unexpected order: %+v", events)
	}
	// Less must be a strict weak order.
	if Less(a, a) || !Less(b, a) || Less(c, b) {
		t.Fatal("Less invariant violated")
	}
}

func TestDigestDistinguishes(t *testing.T) {
	base := Event{Kind: KindSEI, TrackID: 1, Start: 5, Payload: []byte{1}}
	other := base
	other.Payload = []byte{2}
	if base.Digest() == other.Digest() {
		t.Fatal("digest should include payload bytes")
	}
	copied := base
	if base.Digest() != copied.Digest() {
		t.Fatal("digest should be stable")
	}
}

func TestClassify(t *testing.T) {
	err := Classify(ClassCorrupt, ErrTruncated)
	if ClassOf(err) != ClassCorrupt {
		t.Fatalf("class: %v", ClassOf(err))
	}
	// A second wrap must not shadow the original class.
	wrapped := Classify(ClassTransient, err)
	if ClassOf(wrapped) != ClassCorrupt {
		t.Fatalf("rewrapped class: %v", ClassOf(wrapped))
	}
	if !errors.Is(wrapped, ErrTruncated) {
		t.Fatal("errors.Is should see through the wrapper")
	}
	if Classify(ClassCommit, nil) != nil {
		t.Fatal("nil stays nil")
	}
	if ClassOf(errors.New("plain")) != ClassUnknown {
		t.Fatal("plain error should be unknown")
	}
}

func TestEventJSONStable(t *testing.T) {
	e := Event{Kind: KindChapter, TrackID: 3, Start: -1, Duration: 90000, PayloadID: "c1"}
	first, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var back Event
	if err := json.Unmarshal(first, &back); err != nil {
		t.Fatal(err)
	}
	if back.Digest() != e.Digest() {
		t.Fatalf("json roundtrip mismatch: %s vs %s", back.Digest(), e.Digest())
	}
}
