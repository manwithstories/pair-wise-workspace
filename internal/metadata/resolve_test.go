package metadata

import (
	"testing"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

func trackWithClaims(policy, bitstream, container map[string]string) *model.Track {
	return &model.Track{
		ID: 1, Kind: model.Video, Codec: model.CodecMJPEG,
		Claims: model.Claims{Policy: policy, Bitstream: bitstream, Container: container},
	}
}

func TestPrecedencePolicyWins(t *testing.T) {
	tr := trackWithClaims(
		map[string]string{FieldRotation: "180"},
		map[string]string{FieldRotation: "90"},
		map[string]string{FieldRotation: "0"},
	)
	res := Resolve(tr, Options{})
	if tr.Resolved.Rotation != 180 {
		t.Fatalf("rotation = %d, want 180", tr.Resolved.Rotation)
	}
	if res.Sources[FieldRotation] != model.SourcePolicy {
		t.Fatalf("source = %s, want policy", res.Sources[FieldRotation])
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("conflicts = %d, want 1", len(res.Conflicts))
	}
	losers := res.Conflicts[0].Losers
	if len(losers) != 2 || losers[0].Source != model.SourceBitstream || losers[1].Source != model.SourceContainer {
		t.Fatalf("losers = %+v", losers)
	}
}

func TestPrecedenceBitstreamOverContainer(t *testing.T) {
	tr := trackWithClaims(nil,
		map[string]string{FieldPrimaries: "9"},
		map[string]string{FieldPrimaries: "1"},
	)
	Resolve(tr, Options{})
	if tr.Resolved.Primaries != 9 {
		t.Fatalf("primaries = %d, want 9", tr.Resolved.Primaries)
	}
}

func TestPolicyFromOptions(t *testing.T) {
	tr := trackWithClaims(nil, map[string]string{FieldRotation: "90"}, map[string]string{FieldRotation: "0"})
	Resolve(tr, Options{Policy: map[string]string{FieldRotation: "270"}})
	if tr.Resolved.Rotation != 270 {
		t.Fatalf("rotation = %d, want 270", tr.Resolved.Rotation)
	}
}

func TestUnknownColorMarkerIsolatesTrack(t *testing.T) {
	tr := trackWithClaims(nil, nil, map[string]string{FieldColor: "unknown:xxxx"})
	tr.Kind = model.Video
	tr.Resolved.Width, tr.Resolved.Height = 32, 32
	res := Resolve(tr, Options{})
	if !res.Isolated || !tr.Isolated {
		t.Fatal("unknown colour marker must isolate the track")
	}
	found := false
	for _, it := range res.Faults.Items {
		if it.Kind == fault.KindMetadata && it.Recoverable {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a recoverable metadata fault, got %v", res.Faults)
	}
}

func TestCropApertureRotationOrder(t *testing.T) {
	// A 64x48 frame rotated 90 becomes 48x64; a 56x40 aperture must be
	// swapped to 40x56 before centring on the rotated frame.
	tr := trackWithClaims(nil, map[string]string{
		FieldWidth: "64", FieldHeight: "48", FieldAperture: "56x40", FieldRotation: "90",
	}, nil)
	Resolve(tr, Options{})
	want := model.Rect{Left: 4, Top: 4, Right: 4, Bottom: 4}
	if tr.Resolved.Crop != want {
		t.Fatalf("crop = %s, want %s", tr.Resolved.Crop, want)
	}
}

func TestExplicitCropBeatsAperture(t *testing.T) {
	tr := trackWithClaims(nil, map[string]string{
		FieldWidth: "64", FieldHeight: "48", FieldAperture: "32x32", FieldCrop: "1,2,3,4",
	}, nil)
	Resolve(tr, Options{})
	if got := tr.Resolved.Crop; got != (model.Rect{Left: 1, Top: 2, Right: 3, Bottom: 4}) {
		t.Fatalf("crop = %s, want explicit claim", got)
	}
}

func TestCropDiscardedWhenItRemovesEveryPixel(t *testing.T) {
	tr := trackWithClaims(nil, map[string]string{
		FieldWidth: "16", FieldHeight: "16", FieldCrop: "8,0,8,0",
	}, nil)
	res := Resolve(tr, Options{})
	if tr.Resolved.Crop != (model.Rect{}) {
		t.Fatalf("degenerate crop kept: %s", tr.Resolved.Crop)
	}
	if res.Faults.Len() == 0 {
		t.Fatal("degenerate crop must be reported")
	}
}

func TestMalformedClaimIsReported(t *testing.T) {
	tr := trackWithClaims(map[string]string{FieldRotation: "banana"},
		map[string]string{FieldRotation: "90"}, nil)
	res := Resolve(tr, Options{})
	if tr.Resolved.Rotation != 90 {
		t.Fatalf("rotation = %d, want fallback 90", tr.Resolved.Rotation)
	}
	reported := false
	for _, it := range res.Faults.Items {
		if it.Kind == fault.KindMetadata {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("malformed policy claim not reported: %v", res.Faults)
	}
}

func TestRotationUnknownIsFlagged(t *testing.T) {
	tr := trackWithClaims(nil, nil, map[string]string{FieldRotation: "unknown"})
	res := Resolve(tr, Options{})
	if res.Faults.Len() == 0 {
		t.Fatal("unreadable rotation must be flagged")
	}
	if tr.Resolved.Rotation != 0 {
		t.Fatalf("rotation = %d, want 0", tr.Resolved.Rotation)
	}
}
