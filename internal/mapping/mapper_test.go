package mapping

import (
	"testing"

	"mediadeliver/internal/manifest"
	"mediadeliver/internal/model"
)

func rule(line int, seg string, kind model.TrackKind, mat string, tid uint32) *manifest.MappingRule {
	return &manifest.MappingRule{Line: line, Segment: seg, Kind: kind, MaterialID: mat, TrackID: tid}
}

func TestBuildAssignsSlotsAndRoutes(t *testing.T) {
	m := New()
	m.RegisterTrack("cam1", 1, model.KindVideo, "avc1")
	m.RegisterTrack("cam1", 2, model.KindAudio, "mp4a")
	rules := []*manifest.MappingRule{
		rule(10, "segA", model.KindVideo, "cam1", 1),
		rule(11, "segA", model.KindAudio, "cam1", 2),
	}
	out, err := m.Build("segA", rules, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].OutputSlot != 0 || out[1].OutputSlot != 1 {
		t.Fatalf("slots wrong: %+v", out)
	}
	if !m.Accept("segA", "cam1", 1) || m.Accept("segA", "cam1", 9) {
		t.Fatal("accept filter wrong")
	}
	if m.Route("segA", "cam1", 2) != 1 || m.Route("segA", "cam1", 9) != -1 {
		t.Fatal("route wrong")
	}
}

func TestDuplicateMappingRejected(t *testing.T) {
	m := New()
	m.RegisterTrack("cam1", 1, model.KindVideo, "avc1")
	rules := []*manifest.MappingRule{
		rule(10, "segA", model.KindVideo, "cam1", 1),
		rule(12, "segA", model.KindVideo, "cam1", 1),
	}
	_, err := m.Build("segA", rules, false)
	if model.KindOf(err) != model.KindConflict {
		t.Fatalf("want KindConflict, got %v", err)
	}
	if model.PositionOf(err) != 12 {
		t.Fatalf("want line 12, got %d", model.PositionOf(err))
	}
}

func TestIncompatibleCodecRejected(t *testing.T) {
	m := New()
	m.RegisterTrack("cam1", 1, model.KindVideo, "avc1")
	m.RegisterTrack("cam2", 3, model.KindVideo, "hev1")
	rules := []*manifest.MappingRule{
		rule(10, "segA", model.KindVideo, "cam1", 1),
		rule(13, "segA", model.KindVideo, "cam2", 3),
	}
	_, err := m.Build("segA", rules, false)
	if model.KindOf(err) != model.KindInvalidInput {
		t.Fatalf("want KindInvalidInput for codec conflict, got %v", err)
	}
}

func TestKindMismatchRejected(t *testing.T) {
	m := New()
	m.RegisterTrack("cam1", 2, model.KindAudio, "mp4a")
	rules := []*manifest.MappingRule{
		rule(10, "segA", model.KindVideo, "cam1", 2), // rule says video, track is audio
	}
	_, err := m.Build("segA", rules, false)
	if model.KindOf(err) != model.KindInvalidInput {
		t.Fatalf("want KindInvalidInput for kind mismatch, got %v", err)
	}
}

func TestIsolatedDamagedTrackContinuesOthers(t *testing.T) {
	m := New()
	m.RegisterTrack("cam1", 1, model.KindVideo, "avc1")
	m.RegisterTrack("cam1", 2, model.KindAudio, "mp4a")
	m.MarkDamaged("cam1", 1) // video track damaged
	rules := []*manifest.MappingRule{
		rule(10, "segA", model.KindVideo, "cam1", 1),
		rule(11, "segA", model.KindAudio, "cam1", 2),
	}
	out, err := m.Build("segA", rules, true)
	if err != nil {
		t.Fatalf("isolation should not fail the build: %v", err)
	}
	if len(out) != 1 || out[0].SourceTrack != 2 {
		t.Fatalf("only the healthy audio track should remain, got %+v", out)
	}
	if m.Accept("segA", "cam1", 1) {
		t.Fatal("damaged track must be rejected by Accept")
	}
	errs := m.TrackErrors()
	if len(errs) != 1 || model.KindOf(errs[0]) != model.KindCorruptMedia {
		t.Fatalf("want one recorded track error, got %+v", errs)
	}
}

func TestCodecCompat(t *testing.T) {
	if !CodecCompat("", "avc1") || !CodecCompat("avc1", "avc1") {
		t.Fatal("empty/equal codecs must be compatible")
	}
	if CodecCompat("avc1", "hev1") {
		t.Fatal("different codecs must be incompatible")
	}
}
