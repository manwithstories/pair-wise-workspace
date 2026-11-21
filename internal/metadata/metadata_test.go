package metadata

import (
	"bytes"
	"strings"
	"testing"

	"mediasync/internal/model"
)

func TestMXFParseNormalAndDiagnostics(t *testing.T) {
	tracks := []TrackMeta{
		{TrackID: "cam1", Kind: model.KindVideo, Timescale: 25, FrameRate: model.Rational{Num: 25, Den: 1},
			Width: 1280, Height: 720, Rotation: 180, HasRot: true,
			Crop:  &CropRect{X: 10, Y: 10, Width: 1260, Height: 700},
			Color: &ColorDesc{ColorType: "nclx", Primaries: 1, Transfer: 1, Matrix: 1}},
	}
	var buf bytes.Buffer
	if err := WriteMXF(&buf, tracks); err != nil {
		t.Fatal(err)
	}
	res, err := ParseMXF(&buf, "sample.mxf")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tracks) != 1 {
		t.Fatalf("want 1 track, got %d", len(res.Tracks))
	}
	got := res.Tracks[0]
	if got.Timescale != 25 || got.Rotation != 180 || got.Crop == nil || got.Color == nil ||
		got.Color.Primaries != 1 || got.FrameRate.Num != 25 {
		t.Fatalf("normalization mismatch: %+v", got)
	}

	bad := strings.NewReader(mxfHeader + "\ncontainer=mxf\ntrack.id=x\ntrack.timescale=-3\nframe.rate=abc\n")
	res2, err := ParseMXF(bad, "bad.mxf")
	if err != nil {
		t.Fatal(err)
	}
	if res2.Diag.Count(model.KindIllegalField) < 2 {
		t.Fatalf("want >=2 illegal-field diagnostics, got %v", res2.Diag.All())
	}
	if len(res2.Diag.Fatal()) == 0 {
		t.Fatal("illegal timescale must be an unrecoverable diagnostic")
	}

	missing := strings.NewReader(mxfHeader + "\ncontainer=mxf\ntrack.id=y\ntrack.kind=video\n")
	res3, err := ParseMXF(missing, "missing.mxf")
	if err != nil {
		t.Fatal(err)
	}
	if res3.Diag.Count(model.KindMissingField) == 0 {
		t.Fatalf("missing timescale/frame rate must be diagnosed: %v", res3.Diag.All())
	}

	if _, err := ParseMXF(strings.NewReader("wrong header\n"), "noheader.mxf"); err == nil {
		t.Fatal("missing subset header must return an error")
	}
}

func TestMP4RoundTripNormalization(t *testing.T) {
	tracks := []TrackMeta{
		{TrackID: "cam-a", Kind: model.KindVideo, Timescale: 90000, FrameRate: model.Rate2997,
			Width: 1920, Height: 1080, Rotation: 270, HasRot: true,
			Crop:  &CropRect{X: 0, Y: 0, Width: 1920, Height: 1080},
			Color: &ColorDesc{ColorType: "nclx", Primaries: 9, Transfer: 16, Matrix: 9, FullRange: true}},
		{TrackID: "mic", Kind: model.KindAudio, Timescale: 48000,
			FrameRate: model.Rational{Num: 48000, Den: 1}},
	}
	var buf bytes.Buffer
	if err := WriteMP4(&buf, tracks); err != nil {
		t.Fatal(err)
	}
	res, err := ParseMP4(bytes.NewReader(buf.Bytes()), "synthetic.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tracks) != 2 {
		t.Fatalf("want 2 tracks, got %d", len(res.Tracks))
	}
	v := res.Tracks[0]
	if v.Rotation != 270 || v.Crop == nil || v.Color == nil || !v.Color.FullRange {
		t.Fatalf("video fields not normalized: %+v", v)
	}
	if v.FrameRate != model.Rate2997 {
		t.Fatalf("non-integer frame rate lost: got %s", v.FrameRate)
	}
	if v.Timescale != 90000 || v.Width != 1920 || v.Height != 1080 {
		t.Fatalf("tkhd/mdhd fields wrong: %+v", v)
	}
	a := res.Tracks[1]
	if a.Kind != model.KindAudio || a.Timescale != 48000 {
		t.Fatalf("audio track wrong: %+v", a)
	}
	if res.Diag.Len() != 0 {
		t.Fatalf("clean sample must produce no diagnostics: %v", res.Diag.All())
	}
}

func TestPolicyRotationCropColorConflicts(t *testing.T) {
	p := DefaultPolicy()
	records := []TrackMeta{
		{TrackID: "cam1", Source: "mp4", Timescale: 90000, FrameRate: model.Rate30,
			Width: 1920, Height: 1080, HasRot: true, Rotation: 90,
			Crop:  &CropRect{X: 0, Y: 0, Width: 1920, Height: 1080},
			Color: &ColorDesc{ColorType: "nclx", Primaries: 1, Transfer: 1, Matrix: 1}},
		{TrackID: "cam1", Source: "mxf", Timescale: 25, FrameRate: model.Rational{Num: 25, Den: 1},
			Width: 1920, Height: 1080, HasRot: true, Rotation: 45, // non-cardinal -> diagnostic
			Crop:  &CropRect{X: -5, Y: 0, Width: 1920, Height: 1080}, // out of bounds
			Color: &ColorDesc{ColorType: "nclx", Primaries: 9, Transfer: 16, Matrix: 9}},
	}
	resolved, diag := Resolve(p, records)
	// mxf has higher priority; 45 normalizes to 90.
	if resolved.Track.Rotation != 90 {
		t.Fatalf("want normalized rotation 90, got %d", resolved.Track.Rotation)
	}
	if diag.Count(model.KindBadRotation) == 0 {
		t.Fatal("non-cardinal rotation must be diagnosed")
	}
	if diag.Count(model.KindCropOutOfBounds) == 0 {
		t.Fatal("negative-origin crop must be diagnosed as out of bounds")
	}
	if diag.Count(model.KindColorConflict) == 0 {
		t.Fatal("conflicting nclx descriptions must be diagnosed")
	}
	// winner provenance: mxf beats mp4 for rotation/color; crop falls back to mp4.
	fields := map[string]bool{}
	for _, d := range resolved.Decisions {
		fields[d.Field] = true
		switch d.Field {
		case "crop":
			if d.Winner != "mp4" || d.Loser != "mxf" {
				t.Fatalf("crop fallback provenance wrong: %+v", d)
			}
		default:
			if d.Winner != "mxf" || d.Loser != "mp4" {
				t.Fatalf("decision provenance wrong: %+v", d)
			}
		}
	}
	if !fields["rotation"] || !fields["color"] || !fields["crop"] {
		t.Fatalf("expected rotation/color/crop decisions, got %+v", resolved.Decisions)
	}
	if resolved.Track.Crop == nil || resolved.Track.Crop.Width != 1920 {
		t.Fatal("in-bounds mp4 crop should be applied after mxf crop rejection")
	}
	if resolved.Track.Color.Primaries != 9 {
		t.Fatalf("winning color should be mxf nclx(9,..), got %+v", resolved.Track.Color)
	}
}

func TestPolicySidecarWinsAndGrouping(t *testing.T) {
	mp4Res := &ParseResult{Tracks: []TrackMeta{{TrackID: "c1", Source: "mp4",
		HasRot: true, Rotation: 0, Width: 640, Height: 360, FrameRate: model.Rate30, Timescale: 90000}}}
	mxfRes := &ParseResult{Tracks: []TrackMeta{{TrackID: "c1", Source: "mxf",
		HasRot: true, Rotation: 180, Width: 640, Height: 360, FrameRate: model.Rate30, Timescale: 25}}}
	groups := GroupByTrackID(mp4Res, mxfRes)
	if len(groups["c1"]) != 2 {
		t.Fatalf("grouping failed: %+v", groups)
	}
	resolved, _ := Resolve(DefaultPolicy(), groups["c1"])
	if resolved.Track.Rotation != 180 {
		t.Fatalf("mxf rotation should win, got %d", resolved.Track.Rotation)
	}
}
