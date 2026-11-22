package rtp

import (
	"testing"

	"mediakernel/internal/clock"
	"mediakernel/internal/model"
)

func TestMarshalParseRoundTrip(t *testing.T) {
	payload := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	wire, err := Marshal(0xAABBCCDD, 42, 48000, payload)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := Parse(wire, clock.Milliseconds(10), ParseOptions{
		ClockRate: clock.AudioClockRate,
		Kind:      model.KindAudio,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Stream != 0xAABBCCDD || ev.Seq != 42 {
		t.Fatalf("header mismatch: %+v", ev)
	}
	if ev.EventTime != clock.FromRTP(48000, clock.AudioClockRate) {
		t.Fatalf("clock mapping wrong: %v", ev.EventTime)
	}
	if string(ev.AudioPCM) != string(payload) {
		t.Fatal("payload not preserved")
	}
}

func TestMalformedPacketsRejected(t *testing.T) {
	if _, err := Parse([]byte{0x80}, clock.Ticks(0), ParseOptions{ClockRate: 48000, Kind: model.KindAudio}); err == nil {
		t.Fatal("short packet accepted")
	}
	// Valid header but payload over the safety bound.
	big := make([]byte, MaxPayloadBytes+1)
	if _, err := Marshal(1, 1, 0, big); err == nil {
		t.Fatal("marshal must reject oversized payload")
	}
	if _, err := Marshal(1, 1, 0, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
}

func TestTrackerDuplicateConflictAndGap(t *testing.T) {
	log := model.NewLog(64)
	tr := NewTracker(log, 16)
	base := func(seq uint16, hash uint64) *model.Event {
		return &model.Event{Kind: model.KindAudio, Stream: 9, Seq: seq, PayloadHash: hash, EventTime: clock.Ticks(seq)}
	}
	if !tr.Observe(base(10, 100)) {
		t.Fatal("first packet must be accepted")
	}
	// Reordered packet first creates a gap, then the gap fills.
	if !tr.Observe(base(12, 120)) {
		t.Fatal("packet 12 must be accepted")
	}
	if len(tr.Gaps()) != 1 || tr.Gaps()[0].From != 11 || tr.Gaps()[0].To != 11 {
		t.Fatalf("gap detection wrong: %+v", tr.Gaps())
	}
	if !tr.Observe(base(11, 110)) {
		t.Fatal("in-order fill packet rejected")
	}
	// Exact duplicate.
	if tr.Observe(base(10, 100)) {
		t.Fatal("duplicate must not be accepted")
	}
	// Same sequence, different payload -> conflict, isolated.
	if tr.Observe(base(10, 999)) {
		t.Fatal("conflict must be isolated")
	}
	if tr.Duplicates() != 1 || tr.Conflicts() != 1 {
		t.Fatalf("counts wrong dup=%d conf=%d", tr.Duplicates(), tr.Conflicts())
	}
	if log.CountBy(model.CatDuplicate) != 1 || log.CountBy(model.CatConflict) != 1 {
		t.Fatal("fault categories not recorded")
	}
	for _, e := range log.Items() {
		if e.Category == model.CatConflict && e.Decision != model.DecIsolated {
			t.Fatal("conflict not isolated")
		}
	}
}

func TestSequenceWrapGap(t *testing.T) {
	tr := NewTracker(model.NewLog(32), 16)
	tr.Observe(&model.Event{Stream: 1, Seq: 65534, PayloadHash: 1})
	tr.Observe(&model.Event{Stream: 1, Seq: 1, PayloadHash: 2})
	gaps := tr.Gaps()
	if len(gaps) != 1 || gaps[0].From != 65535 || gaps[0].To != 0 {
		t.Fatalf("wrap-around gap wrong: %+v", gaps)
	}
}
