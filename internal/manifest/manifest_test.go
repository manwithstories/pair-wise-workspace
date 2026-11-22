package manifest

import (
	"testing"

	"mediaroute/internal/adapter"
)

func sampleEvents() []adapter.Event {
	return []adapter.Event{
		{Track: "b", Kind: adapter.KindMedia, Seq: 2, TimestampNs: 20, Payload: []byte("y")},
		{Track: "a", Kind: adapter.KindMedia, Seq: 1, TimestampNs: 10, Payload: []byte("x")},
	}
}

func TestBuildAndHitRate(t *testing.T) {
	bounds := []int64{0, 100, 200, 300}
	segEvents := [][]adapter.Event{sampleEvents(), nil, sampleEvents()}
	m := Build(bounds, segEvents, []int64{0, 200})
	if len(m.Segments) != 3 {
		t.Fatalf("segments = %d", len(m.Segments))
	}
	if !m.Segments[0].Keyframe || m.Segments[1].Keyframe || !m.Segments[2].Keyframe {
		t.Fatalf("keyframe flags: %+v", m.Segments)
	}
	if got := m.KeyframeHitRate(); got < 0.66 || got > 0.67 {
		t.Fatalf("hit rate = %f", got)
	}
	if m.Segments[0].Events != 2 || m.Segments[1].Events != 0 {
		t.Fatalf("event counts: %+v", m.Segments)
	}
}

func TestHashDeterministicAndOrderIndependent(t *testing.T) {
	seg := Segment{Index: 0, StartNs: 0, EndNs: 100}
	evs := sampleEvents()
	h1 := HashSegment(seg, evs)
	h2 := HashSegment(seg, []adapter.Event{evs[1], evs[0]})
	if h1 != h2 {
		t.Fatal("hash depends on input order")
	}
	h3 := HashSegment(seg, append(sampleEvents(), adapter.Event{Track: "c", Seq: 9}))
	if h1 == h3 {
		t.Fatal("hash ignores extra event")
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	m := Build([]int64{0, 100}, [][]adapter.Event{sampleEvents()}, []int64{0})
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := WriteSegment(dir, m.Segments[0], sampleEvents()); err != nil {
		t.Fatal(err)
	}
	back, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Segments) != 1 || back.Segments[0].SHA256 != m.Segments[0].SHA256 {
		t.Fatalf("roundtrip mismatch: %+v", back.Segments)
	}
	h, err := HashFile(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if h != m.Segments[0].SHA256 {
		t.Fatalf("file hash %s != manifest %s", h, m.Segments[0].SHA256)
	}
}
