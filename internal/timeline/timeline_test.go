package timeline

import (
	"context"
	"testing"

	"cueforge/internal/clock"
	"cueforge/internal/model"
)

func ev(track int, ms int64, off int64, payload string) model.Event {
	return model.Event{Track: track, Start: clock.Must(ms, 1000),
		Kind: "caption", Payload: payload, Offset: off}
}

func sliceRecv(evs []model.Event) RecvFunc {
	i := 0
	return func(context.Context) (model.Event, bool, error) {
		if i >= len(evs) {
			return model.Event{}, false, nil
		}
		e := evs[i]
		i++
		return e, true, nil
	}
}

func runMerge(t *testing.T, sources [][]model.Event) ([]model.Event, []model.TrackError) {
	t.Helper()
	recvs := make([]RecvFunc, len(sources))
	for i, src := range sources {
		recvs[i] = sliceRecv(src)
	}
	var out []model.Event
	var late []model.TrackError
	err := Merge(context.Background(), recvs,
		func(_ context.Context, e model.Event) error {
			out = append(out, e)
			return nil
		},
		func(te model.TrackError) { late = append(late, te) })
	if err != nil {
		t.Fatal(err)
	}
	return out, late
}

func TestMergeStableOrder(t *testing.T) {
	out, late := runMerge(t, [][]model.Event{
		{ev(0, 0, 10, "a"), ev(0, 4, 30, "d")},
		{ev(1, 0, 5, "b"), ev(1, 2, 20, "c")},
		{ev(2, 0, 3, "e")},
	})
	if len(late) != 0 {
		t.Fatalf("unexpected late events: %v", late)
	}
	// Same-time ties break by track, then source offset.
	want := []struct {
		track int
		ms    int64
	}{{0, 0}, {1, 0}, {2, 0}, {1, 2}, {0, 4}}
	if len(out) != len(want) {
		t.Fatalf("got %d events, want %d", len(out), len(want))
	}
	for i, w := range want {
		if out[i].Track != w.track || out[i].Start.Compare(clock.Must(w.ms, 1000)) != 0 {
			t.Errorf("out[%d] = track %d @ %s, want track %d @ %dms",
				i, out[i].Track, out[i].Start, w.track, w.ms)
		}
	}
}

func TestMergeDeduplicates(t *testing.T) {
	dup := ev(0, 100, 10, "same")
	out, _ := runMerge(t, [][]model.Event{{dup, dup, ev(0, 200, 20, "other")}})
	if len(out) != 2 {
		t.Fatalf("got %d events, want 2 after dedup", len(out))
	}
}

func TestMergeLateEventReported(t *testing.T) {
	// Source 1 is out of order: 50ms arrives after 600ms was emitted.
	out, late := runMerge(t, [][]model.Event{
		{ev(0, 500, 10, "anchor")},
		{ev(1, 600, 15, "ahead"), ev(1, 50, 20, "late")},
	})
	if len(out) != 2 {
		t.Fatalf("got %d emitted, want 2", len(out))
	}
	if len(late) != 1 || late[0].Offset != 20 || late[0].Track != 1 {
		t.Fatalf("late = %+v", late)
	}
	if late[0].Stage != model.StageMerge {
		t.Errorf("stage = %s", late[0].Stage)
	}
}

func TestMergeEmpty(t *testing.T) {
	out, late := runMerge(t, [][]model.Event{{}, {}})
	if len(out) != 0 || len(late) != 0 {
		t.Fatalf("out=%v late=%v", out, late)
	}
}
