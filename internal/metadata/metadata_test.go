package metadata

import (
	"testing"

	"mediaflow/internal/fixture"
)

func sampleEvents() []fixture.Event {
	return []fixture.Event{
		{Track: 5, Kind: fixture.EventID3, RawTS: 100, TrueUS: 100_000, Duration: 0, Payload: []byte("id3-1")},
		{Track: 3, Kind: fixture.EventSubtitle, RawTS: 50, TrueUS: 50_000, Duration: 20_000, Payload: []byte("sub-1")},
		{Track: 5, Kind: fixture.EventEmsg, RawTS: 200, TrueUS: 200_000, Duration: 0, Payload: []byte("emsg-1"), Truncated: true},
		{Track: 4, Kind: fixture.EventChapter, RawTS: 300, TrueUS: 300_000, Duration: 100_000, Payload: []byte("chap-1")},
		{Track: 5, Kind: fixture.EventSEI, RawTS: 400, TrueUS: 400_000, Duration: 0, Payload: []byte("sei-1")},
	}
}

func TestMergeOrdersOnNormalizedTime(t *testing.T) {
	rep, err := Merge(sampleEvents(), 500_000, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(rep.Events))
	}
	for i := 1; i < len(rep.Events); i++ {
		if rep.Events[i].StartUS < rep.Events[i-1].StartUS {
			t.Fatal("events not sorted by normalized time")
		}
	}
	first := rep.Events[0]
	if first.Kind != fixture.EventSubtitle || first.StartUS != 50_000 {
		t.Fatalf("first event wrong: %+v", first)
	}
}

func TestTruncationIsolationAndOffsets(t *testing.T) {
	rep, err := Merge(sampleEvents(), 500_000, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Truncated) != 1 || rep.Truncated[0].Kind != fixture.EventEmsg {
		t.Fatalf("truncation list wrong: %+v", rep.Truncated)
	}
	if len(rep.TruncatedOffs) != 1 || rep.TruncatedOffs[0] != rep.Truncated[0].Offset {
		t.Fatal("truncated first-offset not recorded")
	}
	// The truncated emsg spans [200000,200000); point event isolates a single
	// boundary, so safe segments must exist on both sides.
	if len(rep.Segments) < 2 {
		t.Fatalf("expected safe segments on both sides, got %d: %+v", len(rep.Segments), rep.Segments)
	}
	if rep.IsSafe(150_000) != true {
		t.Fatal("pre-truncation region should be safe")
	}
	if rep.IsSafe(350_000) != true {
		t.Fatal("post-truncation region should be safe")
	}
}

func TestPointTruncationDoesNotSwallowTimeline(t *testing.T) {
	rep, err := Merge(sampleEvents(), 500_000, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	// A zero-duration truncated event must only exclude its single instant;
	// the chapter at 300ms must remain inside a safe segment.
	var chapterSafe bool
	for _, s := range rep.Segments {
		if s.StartUS <= 300_000 && 300_000 < s.EndUS {
			chapterSafe = true
		}
	}
	if !chapterSafe {
		t.Fatalf("chapter swallowed by point-event isolation: %+v", rep.Segments)
	}
}

func TestFindByKind(t *testing.T) {
	rep, _ := Merge(sampleEvents(), 500_000, DefaultConfig())
	id3 := rep.FindByKind(fixture.EventID3)
	if len(id3) != 1 || string(id3[0].Payload) != "id3-1" {
		t.Fatalf("ID3 lookup wrong: %+v", id3)
	}
	if len(rep.FindByKind(fixture.EventSEI)) != 1 {
		t.Fatal("SEI lookup wrong")
	}
}

func TestMergeFromFixtureDeterministic(t *testing.T) {
	clip, err := fixture.Generate(fixture.DefaultConfig(41))
	if err != nil {
		t.Fatal(err)
	}
	clip.Spec.TruncateEvents = []int{1, 5}
	damaged := fixture.ApplyDamage(clip)
	a, err := Merge(damaged.Events, 1_000_000, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Merge(damaged.Events, 1_000_000, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Events) != len(b.Events) || len(a.Segments) != len(b.Segments) {
		t.Fatal("non-deterministic merge")
	}
	if len(a.Truncated) != 2 {
		t.Fatalf("expected 2 truncated, got %d", len(a.Truncated))
	}
}
