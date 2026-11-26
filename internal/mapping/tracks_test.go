package mapping

import (
	"testing"

	"relaycheck/internal/clock"
	"relaycheck/internal/demux"
)

func testInit() *demux.InitInfo {
	return &demux.InitInfo{Tracks: []demux.TrackInfo{
		{ID: 1, Handler: "vide", Codec: "avc1", Timescale: 90000, Width: 640, Height: 360},
		{ID: 2, Handler: "soun", Codec: "mp4a", Timescale: 48000, Language: "eng", Channels: 2},
		{ID: 3, Handler: "soun", Codec: "mp4a", Timescale: 48000, Language: "eng", Channels: 2},
	}}
}

func TestMapperKeepsConflictsSeparate(t *testing.T) {
	m, faults := NewMapper(testInit())
	if len(faults) != 1 || faults[0].Kind != clock.FaultMapConflict {
		t.Fatalf("expected one identity-conflict note, got %v", faults)
	}
	t2, _ := m.Lookup(2)
	t3, _ := m.Lookup(3)
	if t2 == nil || t3 == nil || t2.OutID == t3.OutID {
		t.Fatalf("conflicting tracks must not merge: %v %v", t2, t3)
	}
	if len(m.Tracks()) != 3 {
		t.Fatalf("tracks=%d", len(m.Tracks()))
	}
}

func TestMapperUnknownTrack(t *testing.T) {
	m, _ := NewMapper(testInit())
	if _, fault := m.Lookup(99); fault == nil || fault.Kind != clock.FaultMapConflict {
		t.Fatalf("expected map-conflict, got %v", fault)
	}
}

func TestMapperRejectsZeroAndDuplicateIDs(t *testing.T) {
	init := &demux.InitInfo{Tracks: []demux.TrackInfo{
		{ID: 0, Codec: "avc1"},
		{ID: 1, Codec: "avc1"},
		{ID: 1, Codec: "avc1"},
	}}
	m, faults := NewMapper(init)
	if len(faults) != 2 {
		t.Fatalf("faults=%v", faults)
	}
	if len(m.Tracks()) != 1 {
		t.Fatalf("tracks=%d", len(m.Tracks()))
	}
}

func TestKeyOfDistinguishesLayout(t *testing.T) {
	a := KeyOf(demux.TrackInfo{Codec: "mp4a", Language: "eng", Channels: 2})
	b := KeyOf(demux.TrackInfo{Codec: "mp4a", Language: "eng", Channels: 6})
	c := KeyOf(demux.TrackInfo{Codec: "Opus", Language: "eng", Channels: 2})
	if a == b || a == c {
		t.Fatal("channel layout and codec must distinguish keys")
	}
}
