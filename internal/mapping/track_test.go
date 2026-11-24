package mapping

import (
	"testing"

	"relay/internal/media"
)

func mkFrame(src uint32, kind media.Kind, seq uint64, pts int64) *media.RawFrame {
	return &media.RawFrame{
		Kind: kind,
		Meta: media.FrameMeta{SourceID: src, Sequence: seq, PresentationNS: pts},
	}
}

func TestNewMapperRejectsDuplicates(t *testing.T) {
	_, err := NewMapper([]Rule{
		{SourceID: 1, Kind: media.KindVideo, TrackID: 1},
		{SourceID: 1, Kind: media.KindVideo, TrackID: 2},
	})
	if me := media.AsMediaError(err); me == nil || me.Kind != media.ErrMapping {
		t.Fatalf("want mapping error, got %v", err)
	}
}

func TestNewMapperRejectsTrackConflict(t *testing.T) {
	_, err := NewMapper([]Rule{
		{SourceID: 1, Kind: media.KindVideo, TrackID: 9},
		{SourceID: 2, Kind: media.KindAudio, TrackID: 9},
	})
	if me := media.AsMediaError(err); me == nil || me.Kind != media.ErrMapping {
		t.Fatalf("want track conflict, got %v", err)
	}
}

func TestMapRejectsUnmappedSource(t *testing.T) {
	m, err := NewMapper([]Rule{{SourceID: 1, Kind: media.KindVideo, TrackID: 1}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Map([]*media.RawFrame{mkFrame(2, media.KindVideo, 0, 0)})
	if me := media.AsMediaError(err); me == nil || me.Kind != media.ErrMapping {
		t.Fatalf("want unmapped source error, got %v", err)
	}
}

func TestMapRejectsKindConflict(t *testing.T) {
	m, _ := NewMapper([]Rule{{SourceID: 1, Kind: media.KindVideo, TrackID: 1}})
	_, err := m.Map([]*media.RawFrame{mkFrame(1, media.KindAudio, 0, 0)})
	if me := media.AsMediaError(err); me == nil || me.Kind != media.ErrMapping {
		t.Fatalf("want kind conflict, got %v", err)
	}
}

func TestMapRejectsDuplicateSequence(t *testing.T) {
	m, _ := NewMapper([]Rule{{SourceID: 1, Kind: media.KindVideo, TrackID: 1}})
	_, err := m.Map([]*media.RawFrame{
		mkFrame(1, media.KindVideo, 5, 0),
		mkFrame(1, media.KindVideo, 5, 10),
	})
	if me := media.AsMediaError(err); me == nil || me.Kind != media.ErrMapping {
		t.Fatalf("want duplicate sequence error, got %v", err)
	}
}

func TestMapGroupsAndOrders(t *testing.T) {
	m, err := NewMapper([]Rule{
		{SourceID: 1, Kind: media.KindVideo, TrackID: 1},
		{SourceID: 2, Kind: media.KindAudio, TrackID: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := m.Map([]*media.RawFrame{
		mkFrame(2, media.KindAudio, 0, 30),
		mkFrame(1, media.KindVideo, 0, 20),
		mkFrame(1, media.KindVideo, 1, 10),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("track count: got %d want 2", len(tracks))
	}
	tr1 := tracks[1]
	if tr1.Frames[0].Meta.PresentationNS != 10 || tr1.Frames[1].Meta.PresentationNS != 20 {
		t.Fatal("track 1 not in presentation order")
	}
}

func TestDefaultRulesMixedKindRejected(t *testing.T) {
	_, err := DefaultRules([]*media.RawFrame{
		mkFrame(1, media.KindVideo, 0, 0),
		mkFrame(1, media.KindAudio, 1, 1),
	})
	if me := media.AsMediaError(err); me == nil || me.Kind != media.ErrMapping {
		t.Fatalf("want mixed-kind mapping error, got %v", err)
	}
}
