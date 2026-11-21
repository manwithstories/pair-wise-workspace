package mapping

import (
	"testing"

	"mediameld/internal/model"
)

func mkTrack(id string, kind model.TrackKind, w, h int) *model.Track {
	s := model.Sample{Index: 0, Size: int32(len(id) + 1)}
	copy(s.Hash[:], id)
	return &model.Track{
		ID: id, Kind: kind, Timescale: 90000, SampleDesc: "test",
		CodedWidth: w, CodedHeight: h,
		ContainerColor: model.UnknownColor("none"),
		Samples:        []model.Sample{s},
	}
}

func candidates(tracks ...*model.Track) []Candidate {
	out := make([]Candidate, len(tracks))
	for i, t := range tracks {
		out[i] = Candidate{Input: t.ID, Track: t, Fingerprint: t.Fingerprint()}
	}
	return out
}

func TestResolveBindsByFingerprint(t *testing.T) {
	v := mkTrack("v", model.KindVideo, 640, 360)
	a := mkTrack("a", model.KindAudio, 0, 0)
	cands := candidates(v, a)
	entries := []Entry{
		{Role: RoleMainVideo, Fingerprint: v.Fingerprint()},
		{Role: RoleMainAudio, Fingerprint: a.Fingerprint()},
	}
	res, err := Resolve(entries, cands, Policy{}, DefaultRequirement())
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Track != v {
		t.Fatalf("resolved wrong track: %+v", res)
	}
}

func TestResolveRejectsDuplicateRole(t *testing.T) {
	v := mkTrack("v", model.KindVideo, 640, 360)
	a := mkTrack("a", model.KindAudio, 0, 0)
	cands := candidates(v, a)
	entries := []Entry{
		{Role: RoleMainVideo, Fingerprint: v.Fingerprint()},
		{Role: RoleMainVideo, Fingerprint: a.Fingerprint()},
	}
	_, err := Resolve(entries, cands, Policy{}, DefaultRequirement())
	if err == nil {
		t.Fatal("duplicate role accepted")
	}
	if code, _ := model.CodeOf(err); code != model.CodeTrackDuplicate {
		t.Fatalf("code=%v", code)
	}
}

func TestResolveRejectsDuplicateFingerprint(t *testing.T) {
	v := mkTrack("v", model.KindVideo, 640, 360)
	a := mkTrack("a", model.KindAudio, 0, 0)
	entries := []Entry{
		{Role: RoleMainVideo, Fingerprint: v.Fingerprint()},
		{Role: RoleMainAudio, Fingerprint: v.Fingerprint()},
	}
	_, err := Resolve(entries, candidates(v, a), Policy{}, DefaultRequirement())
	if err == nil {
		t.Fatal("same fingerprint bound to two roles")
	}
}

func TestResolveMissingRequiredAudio(t *testing.T) {
	v := mkTrack("v", model.KindVideo, 640, 360)
	entries := []Entry{{Role: RoleMainVideo, Fingerprint: v.Fingerprint()}}
	_, err := Resolve(entries, candidates(v), Policy{}, DefaultRequirement())
	if err == nil {
		t.Fatal("missing audio accepted")
	}
	if code, _ := model.CodeOf(err); code != model.CodeTrackMissing {
		t.Fatalf("code=%v", code)
	}
}

func TestResolveCropOutOfRange(t *testing.T) {
	v := mkTrack("v", model.KindVideo, 640, 360)
	a := mkTrack("a", model.KindAudio, 0, 0)
	bad := model.Crop{Valid: true, X: 600, Y: 0, W: 100, H: 100}
	entries := []Entry{
		{Role: RoleMainVideo, Fingerprint: v.Fingerprint()},
		{Role: RoleMainAudio, Fingerprint: a.Fingerprint()},
	}
	_, err := Resolve(entries, candidates(v, a), Policy{Crop: &bad}, DefaultRequirement())
	if err == nil {
		t.Fatal("out-of-range crop accepted")
	}
	if code, _ := model.CodeOf(err); code != model.CodeCropRange {
		t.Fatalf("code=%v err=%v", code, err)
	}
}

func TestPolicyBeatsTrackBeatsContainer(t *testing.T) {
	v := mkTrack("v", model.KindVideo, 640, 360)
	v.ContainerRotation = 90
	v.TrackRotation = 180
	rot := 270
	d := adjudicate(v, Policy{Rotation: &rot})
	if d.Rotation != 270 {
		t.Fatalf("rotation=%d want 270", d.Rotation)
	}
	// The trail must include all three layers in order.
	var layers []string
	for _, r := range d.Reasons {
		if r.Field == "rotation" {
			layers = append(layers, r.Source)
		}
	}
	want := []string{"container", "track", "policy"}
	if len(layers) != 3 || layers[0] != want[0] || layers[1] != want[1] || layers[2] != want[2] {
		t.Fatalf("layers=%v want %v", layers, want)
	}
}

func TestUnknownColorNeverOverwritesKnownTrackColor(t *testing.T) {
	v := mkTrack("v", model.KindVideo, 640, 360)
	v.ContainerColor = model.ColorDesc{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", FullRange: "limited"}
	v.TrackColor = model.UnknownColor("zzzz")
	d := adjudicate(v, Policy{})
	if d.Color.Primaries != "bt709" {
		t.Fatalf("known colour overwritten by unknown: %+v", d.Color)
	}
	if len(v.Samples) == 0 {
		t.Fatal("samples were touched by adjudication")
	}
}

func TestBothUnknownStaysUnknown(t *testing.T) {
	v := mkTrack("v", model.KindVideo, 640, 360)
	v.ContainerColor = model.UnknownColor("aaaa")
	v.TrackColor = model.UnknownColor("bbbb")
	d := adjudicate(v, Policy{})
	if d.Color.Primaries != "unknown" {
		t.Fatalf("expected unknown, got %+v", d.Color)
	}
}
